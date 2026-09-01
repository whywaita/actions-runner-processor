# Changelog

## [v0.0.8](https://github.com/whywaita/actions-runner-processor/compare/v0.0.7...v0.0.8) - 2026-09-01

- Fix docker container jobs failing with bpf_prog_query(BPF_CGROUP_DEVICE) operation not permitted by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/27

## [v0.0.7](https://github.com/whywaita/actions-runner-processor/compare/v0.0.6...v0.0.7) - 2026-09-01

- ci: run the full image build alongside each GitHub Release by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/19
- fix: restore sudo setuid bit and docker group in runner image by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/23
- fix(ci): dispatch full-image build from goreleaser (release.yaml) by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/24
- fix(ci): grant actions:write so goreleaser can dispatch build-image-full by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/25
- feat(setup): one-shot host bootstrap via `actions-runner-processor setup` by @whywaita in https://github.com/whywaita/actions-runner-processor/pull/26

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
