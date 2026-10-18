"""The owner's `agent` CLI: `agent login` and `agent ask`.

It calls InvokeAgentRuntime with the owner's own AWS credentials (SigV4)
and the owner user id, and never sees a JMAP token. `agent login` serves a
one-shot callback on localhost: after Cognito consent the token vault
redirects there with a session id, and the CLI completes the session
binding with CompleteResourceTokenAuth for the same user id.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import uuid
import webbrowser
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any
from urllib.parse import parse_qs, urlsplit

import boto3

from .config import OWNER_USER_ID

DEFAULT_RETURN_URL = "http://localhost:8765/callback"
LOGIN_TIMEOUT_SECONDS = 300


def new_session_id() -> str:
    # Runtime session ids need at least 33 characters.
    return f"cli-{uuid.uuid4()}"


class Runtime:
    def __init__(self, arn: str, *, client: Any | None = None, qualifier: str = "live") -> None:
        self.arn = arn
        self.qualifier = qualifier
        self.client = client or boto3.client("bedrock-agentcore", region_name=arn.split(":")[3])

    def invoke(self, payload: dict[str, Any], session_id: str) -> dict[str, Any]:
        response = self.client.invoke_agent_runtime(
            agentRuntimeArn=self.arn,
            qualifier=self.qualifier,
            runtimeSessionId=session_id,
            runtimeUserId=OWNER_USER_ID,
            contentType="application/json",
            accept="application/json",
            payload=json.dumps(payload).encode(),
        )
        result: dict[str, Any] = json.loads(response["response"].read())
        return result

    def complete_login(self, session_uri: str) -> None:
        self.client.complete_resource_token_auth(
            sessionUri=session_uri, userIdentifier={"userId": OWNER_USER_ID}
        )


def session_from_callback(path: str, expected_path: str) -> str | None:
    parts = urlsplit(path)
    if parts.path != expected_path:
        return None
    values = parse_qs(parts.query).get("session_id")
    return values[0] if values else None


def wait_for_callback(return_url: str, timeout: float) -> str | None:
    """Serve one request on the localhost return URL; return its session id."""
    target = urlsplit(return_url)
    if target.hostname not in ("localhost", "127.0.0.1") or target.scheme != "http":
        raise SystemExit("the return URL must be http://localhost")
    found: list[str] = []

    class Callback(BaseHTTPRequestHandler):
        def do_GET(self) -> None:
            session = session_from_callback(self.path, target.path)
            if session:
                found.append(session)
            self.send_response(200 if session else 400)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.end_headers()
            message = (
                "Login received; return to the terminal." if session else "Unexpected request."
            )
            self.wfile.write(message.encode())

        def log_message(self, format: str, *args: Any) -> None:
            pass

    deadline = time.monotonic() + timeout
    with HTTPServer(("127.0.0.1", target.port or 80), Callback) as server:
        while not found:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            server.timeout = remaining
            server.handle_request()
    return found[0] if found else None


def login(runtime: Runtime, return_url: str, *, open_browser: bool = True) -> int:
    result = runtime.invoke({"mode": "login"}, new_session_id())
    if result.get("status") == "ok":
        print("Already authorized.")
        return 0
    url = result.get("authorization_url")
    if result.get("status") != "authorization_required" or not url:
        print(f"login failed: {result.get('error', result.get('status'))}", file=sys.stderr)
        return 1
    print(f"Open this URL to sign in:\n{url}")
    if open_browser:
        webbrowser.open(url)
    session_uri = wait_for_callback(return_url, LOGIN_TIMEOUT_SECONDS)
    if not session_uri:
        print("login timed out", file=sys.stderr)
        return 1
    runtime.complete_login(session_uri)
    confirmed = runtime.invoke({"mode": "login"}, new_session_id())
    if confirmed.get("status") != "ok":
        print(f"login not confirmed: {confirmed.get('status')}", file=sys.stderr)
        return 1
    print("Authorized.")
    return 0


def ask(runtime: Runtime, question: str, thread: str | None) -> int:
    session_id = thread or new_session_id()
    result = runtime.invoke({"mode": "ask", "question": question}, session_id)
    status = result.get("status")
    if status == "authorization_required":
        print("Not authorized: run `agent login`.", file=sys.stderr)
        return 2
    if status != "ok":
        print(f"error: {result.get('error', status)}", file=sys.stderr)
        return 1
    print(result.get("answer") or "")
    print(f"\nthread: {session_id} (follow up with --thread)", file=sys.stderr)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="agent", description="s3mail read-only assistant")
    parser.add_argument(
        "--runtime-arn",
        default=os.environ.get("S3MAIL_AGENT_RUNTIME_ARN"),
        help="AgentCore runtime ARN (infra/agent output runtime_arn); "
        "default $S3MAIL_AGENT_RUNTIME_ARN",
    )
    sub = parser.add_subparsers(dest="command", required=True)
    p_login = sub.add_parser("login", help="consent once so the token vault holds your tokens")
    p_login.add_argument("--return-url", default=DEFAULT_RETURN_URL)
    p_login.add_argument("--no-browser", action="store_true")
    p_ask = sub.add_parser("ask", help="ask a question about your mail")
    p_ask.add_argument("question")
    p_ask.add_argument("--thread", help="continue an earlier conversation")
    args = parser.parse_args(argv)
    if not args.runtime_arn:
        parser.error("set --runtime-arn or S3MAIL_AGENT_RUNTIME_ARN")
    runtime = Runtime(args.runtime_arn)
    if args.command == "login":
        return login(runtime, args.return_url, open_browser=not args.no_browser)
    return ask(runtime, args.question, args.thread)


if __name__ == "__main__":
    sys.exit(main())
