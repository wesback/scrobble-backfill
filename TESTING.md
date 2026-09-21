# Testing

Run the repository's single test command from the repository root:

```sh
go test ./...
```

This is also the command used by continuous integration and by canary
verification.

`internal/harness` is a placeholder package that exists only so this
command has something real to run against a module that otherwise has no
code yet. Delete it once a real package's tests can carry that job.
