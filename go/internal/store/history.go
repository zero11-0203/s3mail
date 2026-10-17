package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// HistoryRetention is how long change history is kept before the history
// floor may pass it (docs/specs/storage-sync.md#dynamodb-storage-and-consistency-contract).
const HistoryRetention = 30 * 24 * time.Hour

// FloorGrace is how long a floor is in force before the records under it
// are deleted, so a read that saw the previous floor still finds them.
const FloorGrace = time.Hour

// HistoryWriter is the subset of the DynamoDB client history pruning
// writes with.
type HistoryWriter interface {
	UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

// Pruned reports one history pruning run.
type Pruned struct {
	// Deleted counts the CHANGE records removed under the previous floor.
	Deleted int
	// Floor is the history floor after the run.
	Floor int64
}

// PruneHistory deletes the CHANGE records at or below a history floor
// that has been in force for FloorGrace, then advances the floor to the
// newest change older than HistoryRetention, never past keep (the oldest
// sequence a change reader still needs, the registered search index's).
// The floor moves under the STATE sequence guard; records under the new
// floor are deleted by a later run. JMAP rejects older states with
// cannotCalculateChanges.
func (a *Account) PruneHistory(ctx context.Context, w HistoryWriter, keep int64) (Pruned, error) {
	var out Pruned
	for attempt := 0; ; attempt++ {
		cur, err := a.DB.GetItem(ctx, &dynamodb.GetItemInput{TableName: &a.Table, Key: a.key("STATE"), ConsistentRead: aws.Bool(true)})
		if err != nil {
			return out, err
		}
		seq, _ := num(cur.Item["seq"])
		floor, _ := num(cur.Item["historyFloor"])
		setAt, _ := num(cur.Item["floorAt"])
		out.Floor = floor
		if attempt == 0 && floor > 0 && a.now().Sub(time.Unix(setAt, 0)) >= FloorGrace {
			if out.Deleted, err = a.deleteChanges(ctx, w, floor); err != nil {
				return out, err
			}
		}
		next, err := a.floorCandidate(ctx, floor, min(keep, seq))
		if err != nil || next <= floor {
			return out, err
		}
		_, err = w.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:           &a.Table,
			Key:                 a.key("STATE"),
			UpdateExpression:    aws.String("SET historyFloor = :next, floorAt = :now"),
			ConditionExpression: aws.String("seq = :seq"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":next": n(next), ":now": n(a.now().Unix()), ":seq": n(seq),
			},
		})
		var ccf *types.ConditionalCheckFailedException
		if errors.As(err, &ccf) && attempt < 2 {
			continue
		}
		if err != nil {
			return out, err
		}
		out.Floor = next
		return out, nil
	}
}

// floorCandidate returns the newest change in (floor, upto] committed
// before the retention cutoff, or floor when there is none.
func (a *Account) floorCandidate(ctx context.Context, floor, upto int64) (int64, error) {
	if upto <= floor {
		return floor, nil
	}
	cutoff := a.now().Add(-HistoryRetention).Unix()
	found := floor
	var start map[string]types.AttributeValue
	for {
		page, err := a.DB.Query(ctx, &dynamodb.QueryInput{
			TableName:                &a.Table,
			ConsistentRead:           aws.Bool(true),
			KeyConditionExpression:   aws.String("PK = :pk AND SK BETWEEN :from AND :to"),
			ProjectionExpression:     aws.String("SK, #at"),
			ExpressionAttributeNames: map[string]string{"#at": "at"},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": s(a.pk()), ":from": s("CHANGE#" + padded(floor+1)), ":to": s("CHANGE#" + padded(upto)),
			},
			ExclusiveStartKey: start,
		})
		if err != nil {
			return 0, err
		}
		for _, it := range page.Items {
			at, _ := num(it["at"])
			if at >= cutoff {
				return found, nil
			}
			if found, err = changeSeq(str(it["SK"])); err != nil {
				return 0, err
			}
		}
		if len(page.LastEvaluatedKey) == 0 {
			return found, nil
		}
		start = page.LastEvaluatedKey
	}
}

// deleteChanges removes every CHANGE record at or below floor.
func (a *Account) deleteChanges(ctx context.Context, w HistoryWriter, floor int64) (int, error) {
	var keys []string
	err := a.queryRange(ctx, "CHANGE#"+padded(0), "CHANGE#"+padded(floor), func(sk string) { keys = append(keys, sk) })
	if err != nil {
		return 0, err
	}
	err = forEach(ctx, keys, 8, func(ctx context.Context, sk string) error {
		_, err := w.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &a.Table, Key: a.key(sk)})
		return err
	})
	return len(keys), err
}

func (a *Account) queryRange(ctx context.Context, from, to string, each func(sk string)) error {
	var start map[string]types.AttributeValue
	for {
		page, err := a.DB.Query(ctx, &dynamodb.QueryInput{
			TableName:              &a.Table,
			KeyConditionExpression: aws.String("PK = :pk AND SK BETWEEN :from AND :to"),
			ProjectionExpression:   aws.String("SK"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": s(a.pk()), ":from": s(from), ":to": s(to),
			},
			ExclusiveStartKey: start,
		})
		if err != nil {
			return err
		}
		for _, it := range page.Items {
			each(str(it["SK"]))
		}
		if len(page.LastEvaluatedKey) == 0 {
			return nil
		}
		start = page.LastEvaluatedKey
	}
}

// EmailDestroyedSince reports whether a change at or below upto that was
// committed at or after since destroyed an Email.
func (a *Account) EmailDestroyedSince(ctx context.Context, since time.Time, upto int64) (bool, error) {
	var start map[string]types.AttributeValue
	for {
		page, err := a.DB.Query(ctx, &dynamodb.QueryInput{
			TableName:                &a.Table,
			KeyConditionExpression:   aws.String("PK = :pk AND SK BETWEEN :from AND :to"),
			ProjectionExpression:     aws.String("#at, destroyed"),
			ExpressionAttributeNames: map[string]string{"#at": "at"},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": s(a.pk()), ":from": s("CHANGE#" + padded(0)), ":to": s("CHANGE#" + padded(upto)),
			},
			ScanIndexForward:  aws.Bool(false),
			ExclusiveStartKey: start,
		})
		if err != nil {
			return false, err
		}
		for _, it := range page.Items {
			if at, _ := num(it["at"]); at < since.Unix() {
				return false, nil
			}
			if len(idsByType(it["destroyed"])["Email"]) > 0 {
				return true, nil
			}
		}
		if len(page.LastEvaluatedKey) == 0 {
			return false, nil
		}
		start = page.LastEvaluatedKey
	}
}

func changeSeq(sk string) (int64, error) {
	seq, err := strconv.ParseInt(strings.TrimPrefix(sk, "CHANGE#"), 10, 64)
	if err != nil {
		return 0, errors.New("malformed change key " + strconv.Quote(sk))
	}
	return seq, nil
}
