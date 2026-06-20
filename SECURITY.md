# Security Policy

`comment-cli` is the local daemon and CLI for Comment.io. It runs on your machine,
holds local credentials, and exposes a local control interface — so its security
model is worth understanding.

## Local control surface

- The daemon listens on a **Unix domain socket** (`$COMMENT_IO_HOME/daemon.sock`,
  mode `0600`) and authenticates callers by **SO_PEERCRED UID match** plus a
  **capability token** (`owner.cap`, or a per-session capability).
- An **opt-in TCP transport** (`COMMENT_IO_BUS_TCP_LISTEN` / `COMMENT_IO_BUS_TCP_ADDR`)
  exists for environments where the Unix socket can't be reached (e.g. a
  containerized daemon on macOS). TCP has no peer credentials, so:
  - the capability token is the sole authorization;
  - a **server-auth handshake** makes the daemon prove it holds the capability
    (`HMAC(capability, nonce)`) before the client sends it, so an impostor on the
    address can't harvest the token;
  - it should be published to **loopback only**, and binding a non-loopback
    address requires an explicit `COMMENT_IO_BUS_TCP_ALLOW_NONLOOPBACK=1` opt-in.
- A single advisory **lock** (`$COMMENT_IO_HOME/daemon.lock`) prevents two daemons
  from running against the same state directory.

## Credentials

Credentials live under `$COMMENT_IO_HOME` (default `~/.comment-io`) with
owner-only permissions (`0700` dirs, `0600` files): agent secrets in `agents/`,
the local capability in `bus/capabilities/`, and local secrets in `.secrets`.
Treat that directory as sensitive — anyone who can read it controls the daemon.

## Reporting a vulnerability

Please report security issues privately to **security@comment.io** rather than
opening a public issue. Include a description, affected version (`comment version`),
and reproduction steps. We aim to acknowledge within a few business days.

Do not include real secrets (tokens, `owner.cap`, the contents of `.secrets`) in a
report.
