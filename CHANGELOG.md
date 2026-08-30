# Changelog

## [v0.0.6](https://github.com/whywaita/actions-runner-processor/compare/v0.0.5...v0.0.6) - 2026-08-30

- feat: distribute the full runner image as split release parts by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/17

## [v0.0.5](https://github.com/whywaita/actions-runner-processor/compare/v0.0.4...v0.0.5) - 2026-08-26

- feat: switch sandbox backend from bubblewrap to systemd-nspawn (custom image + sudo) by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/11
- ci: trigger release pipeline on v* tag pushes for RC candidates by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/13
- fix: run goreleaser on tag pushes despite skipped tagpr job by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/14
- ci: ignore .release/ generated during the release build by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/15
- ci: build the lightweight image outside the repo for tag releases by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/16

## [v0.0.4](https://github.com/whywaita/actions-runner-processor/compare/v0.0.3...v0.0.4) - 2026-08-11

- fix: monitor runner process lifecycle and add JSON log support by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/9

## [v0.0.3](https://github.com/whywaita/actions-runner-processor/compare/v0.0.2...v0.0.3) - 2026-08-10

- fix: remove duplicate Account.Login from repo scope URL by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/7

## [v0.0.2](https://github.com/whywaita/actions-runner-processor/compare/v0.0.1...v0.0.2) - 2026-08-09

- fix: support personal accounts via per-repo scope expansion by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/5

## [v0.0.1](https://github.com/whywaita/actions-runner-processor/commits/v0.0.1) - 2026-08-09

- docs: add DESIGN.md — initial architecture and design by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/1
- feat: Phase 1 — Go module, config, Installation discovery by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/2
