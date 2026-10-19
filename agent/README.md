# s3mail agent

The optional mail assistant of the [agent spec](../docs/specs/agent.md): a LangGraph graph on Amazon Bedrock AgentCore Runtime, with models from OpenRouter, that answers questions about the owner's mail, triages incoming mail, writes a daily digest, drafts replies and, after the owner approves each one, organises mail and sends reviewed drafts. It reaches mail only through the s3mail JMAP API with the owner's OAuth token from the AgentCore Identity token vault, and every answer cites the emails it read.

**Status:** A0–A3 and the A4 agent tools are implemented. A fresh deployment registers the agent's client with `mail.read` only, so drafts, the digest as mail and approved actions answer `scope_not_granted` until you [grant their scopes](../docs/operations/deployment.md#granting-agent-scopes), and the A4 tools stay off until you enable them. No [gate](../docs/specs/agent.md#acceptance-checks) has passed in full; the agent spec records what the reference deployment has granted.

## Layout

| Path | Contents |
| --- | --- |
| `src/s3mail_agent/jmap.py` | JMAP client: session discovery, batched calls with result references, read retries, downloads with a size cap; refuses any method or argument outside the mode's allowlist and never retries a write |
| `src/s3mail_agent/tools.py` | Read tools (`search_mail`, `read_email`, `read_thread`, `list_mailboxes`, and from A4 `semantic_search`): typed arguments, text bodies capped at 32 KiB, output wrapped as untrusted data |
| `src/s3mail_agent/attachments.py` | A4 `read_attachment`: PDF, Office and plain-text extraction within size, page and time caps |
| `src/s3mail_agent/graph.py` | The `StateGraph` for `ask` and `act`: code router, model and tool loop under budgets, evidence check, approval interrupt and executor |
| `src/s3mail_agent/triage.py` | Event-driven triage (A1) into the agent table |
| `src/s3mail_agent/digest.py` | The digest (A1) and its delivery as an email (A2) |
| `src/s3mail_agent/drafts.py` | Reply drafts (A2): headers built by code, the body by the model, the Drafts guard |
| `src/s3mail_agent/proposals.py` | A3 proposals, their hashes and the once-only executor with the send marker |
| `src/s3mail_agent/preferences.py` | Owner-stated preferences as AgentCore Memory records |
| `src/s3mail_agent/store.py` | The agent table (DynamoDB): triage rows, digest cursor, send markers |
| `src/s3mail_agent/metering.py` | Per-run model-call and token budgets |
| `src/s3mail_agent/citations.py` | The answer evidence markers and their check |
| `src/s3mail_agent/app.py` | AgentCore entrypoint: the mode router and scope checks, content-free logs, Memory checkpointer |
| `src/s3mail_agent/models.py` | The one model factory: OpenRouter's OpenAI-compatible API with `data_collection: deny` routing; reads the owner-set API key |
| `src/s3mail_agent/identity.py` | Owner token from the vault; returns the consent URL instead of polling |
| `evals/` | Synthetic evaluation mailbox, the question, injection and triage sets, and the runner |
| `tests/` | Offline tests against fake and stateful JMAP servers, a synthetic `example.com` mailbox and a scripted model |

The trigger Lambda that starts triage runs and the owner's `agent` CLI are Go: `go/cmd/agent-trigger` and `go/cmd/agent`.

## Develop

Needs [uv](https://docs.astral.sh/uv/); Python 3.12 is installed by uv if missing.

```sh
make agent-check     # from the repository root: ruff, mypy and pytest
```

The tests use no AWS account, model or network.

The [evaluation suites](../docs/specs/agent.md#evaluation) in `evals/` run against synthetic mail with the real model. The A0 suite (50 questions and 30 injection attempts) costs about 0.40 USD per full run; the A1 triage set (150 emails, `openai/gpt-6-luna`) about 0.02 USD; the A2 draft set (30 threads, drafted and judged by the ask model) about 0.05 USD:

```sh
make agent-eval                                   # A0; key from the s3mail-agent/model-api-key secret
make agent-eval EVAL_ARGS="--suite triage"        # A1 triage set
make agent-eval EVAL_ARGS="--suite drafts"        # A2 drafts; also writes a Markdown spot-check file
make agent-eval EVAL_ARGS="--only q01 i00"        # a subset; OPENROUTER_API_KEY overrides the secret
```

## Deploy and use

Follow [agent deployment](../docs/operations/deployment.md#agent-deployment). Then, with AWS credentials allowed to invoke the runtime:

```sh
export S3MAIL_AGENT_RUNTIME_ARN=$(terraform -chdir=infra/agent output -raw runtime_arn)
(cd go && go install ./cmd/agent)     # the owner CLI is Go: go/cmd/agent
agent login
agent ask "What did the billing emails this month say?"
agent ask -thread <id> "Which one is still unpaid?"
agent digest -peek                    # what arrived since the last digest, without moving the cursor
agent remember "Sign replies with just my first name."
agent draft <email id> "Accept, and suggest Thursday instead."   # needs mail.write
agent act "Archive the newsletters from last week."              # needs mail.write; prints an approval id
agent approve <approval id>                                      # or: agent reject <approval id>
```
