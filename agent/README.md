# s3mail agent

The optional, read-only mail assistant (phase A0 of the [agent spec](../docs/specs/agent.md)): a LangGraph graph on Amazon Bedrock AgentCore Runtime that answers questions about the owner's mail. It reaches mail only through the s3mail JMAP API with the owner's OAuth token from the AgentCore Identity token vault, and every answer cites the emails it read.

**Status:** deployed on 2026-10-18, ahead of the mail system's [Phase 3 cutover](../docs/roadmap.md#agent-phases); the [A0 gate](../docs/specs/agent.md#a0-gate-mvp) has not passed.

## Layout

| Path | Contents |
| --- | --- |
| `src/s3mail_agent/jmap.py` | Read-only JMAP client: session discovery, batched calls with result references, read retries; refuses any method or argument outside the allowlist |
| `src/s3mail_agent/tools.py` | `search_mail`, `read_email`, `read_thread`, `list_mailboxes`: typed arguments, text bodies capped at 32 KiB, output wrapped as untrusted data |
| `src/s3mail_agent/graph.py` | The `StateGraph`: code router, model and tool loop under step, tool-call and token budgets, citation check |
| `src/s3mail_agent/app.py` | AgentCore entrypoint (`login` and `ask` modes), content-free logs, Memory checkpointer |
| `src/s3mail_agent/identity.py` | Owner token from the vault; returns the consent URL instead of polling |
| `src/s3mail_agent/cli.py` | The owner's `agent` CLI |
| `tests/` | Offline tests against a fake JMAP server, a synthetic `example.com` mailbox and a scripted model |

## Develop

Needs [uv](https://docs.astral.sh/uv/); Python 3.12 is installed by uv if missing.

```sh
make agent-check     # from the repository root: ruff, mypy and pytest
```

The tests use no AWS account, model or network. The question, injection and telemetry evaluations of the [A0 gate](../docs/specs/agent.md#a0-gate-mvp) need a deployed runtime and the synthetic account; they are not built yet.

## Deploy and use

Follow [agent deployment](../docs/operations/deployment.md#agent-deployment). Then, with AWS credentials allowed to invoke the runtime:

```sh
export S3MAIL_AGENT_RUNTIME_ARN=$(terraform -chdir=infra/agent output -raw runtime_arn)
uv run --project agent agent login
uv run --project agent agent ask "What did the billing emails this month say?"
uv run --project agent agent ask --thread <id> "Which one is still unpaid?"
```
