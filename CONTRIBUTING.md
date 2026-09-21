# Contributing

Thanks for your interest in contributing to vmbackup-partition.

Guidelines
- Open issues for bugs or feature requests, and attach the plan JSON produced by `-dryRun -planOut` where relevant.
- Keep changes small and focused. Add tests when appropriate.
- Run `make check` (`gofmt` + `go vet` + `go test`) before submitting a PR.
- Run the synthetic end-to-end suite (`VMBIN=<vm-bin> bash scripts/e2e-synthetic.sh`) for any change touching the filtering or orchestration path.

Design constraints (please keep them)
- Do not fork or copy upstream `lib/backup/**` code — import it.
- Keep the filtering strictly source-side so the produced backup stays restorable by the official `vmrestore`.
- Do not add runtime dependencies to `internal/monthfilter`.

Code of conduct
- Be respectful and provide constructive feedback.
