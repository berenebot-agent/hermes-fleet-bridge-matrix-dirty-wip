# Hermes Fleet Bridge Matrix (dirty WIP)

A planned **single-container, self-hosted Matrix + Element chat service and Hermes fleet controller**. It replaces Telegram chat for existing Hermes Docker Compose agents while preserving wake-on-message and idle container shutdown.

**Status: specification / prototype planning only. No working Matrix implementation has been committed yet.**

## The non-negotiable user experience

After the operator deploys and configures the service, a user needs only:

1. The HTTPS chat URL.
2. A username.
3. A password.

The user logs in and immediately sees provisioned Hermes agent rooms. **No Matrix homeserver selection, account creation, room invitations, permission setup, `@mentions`, device verification, bot registration, or plugins.**

## Planned design

- **Single Docker image and Compose service** containing a Matrix homeserver (proposed: Tuwunel), bundled Element Web and a Go controller.
- **One hostname** behind an existing HTTPS reverse proxy such as Nginx Proxy Manager.
- **Operator-defined Hermes Compose directories** drive automatic bot, account and private-room provisioning.
- **One room per agent, threads as separate Hermes sessions.**
- **Controller-owned reliable Matrix events and Hermes API runs**: wake stopped agent on message, queue and deliver reliably, relay replies/typing, stop managed containers after inactivity.
- **Private by default**: no public registration or federation for v1. No end-to-end encryption in v1.
- **Command parity is a verified requirement**, not an assumption: status/help, model switching, context inspection and cancellation must be checked against actual Hermes HTTP APIs.

**[Read the full functional specification](docs/functional-spec.md)** for deployment assumptions, bootstrapping, permissions, reliability requirements, API feasibility checks and acceptance tests.

## Project layout

```text
.
├── README.md
└── docs/
    └── functional-spec.md
```

## Status and next steps

This is a **separate** project from the existing [Telegram Hermes Fleet Bridge](https://github.com/berenebot-agent/hermes-fleet-bridge-dirty-wip). Start by proving the one-container Matrix + Element login-only experience, then automated agent/room provisioning, durable delivery and container lifecycle management.

Deployment commands will be added only after the application is implemented and tested.
