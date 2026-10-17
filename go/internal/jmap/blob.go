package jmap

import "strings"

// Blob ids name the bytes they resolve to (docs/specs/jmap.md#blobs):
// G<core> is an Email's raw message and P<core>-<part> one of its parts,
// where part is a dot-separated partId path; U<core> is an upload and
// V<core>-<part> a part of an uploaded message.
type blobRef struct {
	// Owner is the Email id (M...) or upload id (U...).
	Owner string
	// Part is "" for the whole blob.
	Part string
}

func (b blobRef) upload() bool { return strings.HasPrefix(b.Owner, "U") }

func rawBlobID(emailID string) string { return "G" + emailID[1:] }

func partBlobID(emailID, partID string) string {
	if strings.HasPrefix(emailID, "U") {
		return "V" + emailID[1:] + "-" + partID
	}
	return "P" + emailID[1:] + "-" + partID
}

// ParseBlobID decodes a blob id issued by this server.
func ParseBlobID(blobID string) (blobRef, bool) {
	if len(blobID) < 2 {
		return blobRef{}, false
	}
	owner := map[byte]string{'G': "M", 'P': "M", 'U': "U", 'V': "U"}[blobID[0]]
	if owner == "" {
		return blobRef{}, false
	}
	switch blobID[0] {
	case 'G', 'U':
		if validCore(blobID[1:]) {
			return blobRef{Owner: owner + blobID[1:]}, true
		}
	default:
		core, part, found := strings.Cut(blobID[1:], "-")
		if found && validCore(core) && validPartID(part) {
			return blobRef{Owner: owner + core, Part: part}, true
		}
	}
	return blobRef{}, false
}

func validPartID(p string) bool {
	if p == "" || len(p) > 64 {
		return false
	}
	for _, seg := range strings.Split(p, ".") {
		if seg == "" || strings.Trim(seg, "0123456789") != "" {
			return false
		}
	}
	return true
}
