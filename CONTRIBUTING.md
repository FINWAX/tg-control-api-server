# Contributing

Thanks for your interest in improving Telegram Control API Server.

## Development

Everything builds and tests in containers — no local Go/Node toolchain needed:

```sh
./scripts/test.sh              # unit tests
./scripts/test-integration.sh  # Postgres-backed store tests
docker compose up -d --build   # run the stack locally
```

See the [README](README.md) for setup and architecture.

## Pull requests

- Keep changes focused: one concern per PR.
- Match the surrounding style; run `gofmt` on Go files.
- Add or update tests for behavior changes; CI must pass.
- Commits follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`, …).

## License

By contributing, you agree that your contributions are licensed under the
Apache License 2.0, the same as the project (see [LICENSE](LICENSE)).
