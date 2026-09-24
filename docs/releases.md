# Rescrobble releases

The supported MVP distribution channel is **GitHub Releases**. Download the
binary for your platform from the release assets, make it executable where
required, and invoke it from a command line.

The MVP does not advertise or support installation through Homebrew, Scoop, or
Docker. Those distribution channels are intentionally deferred.

## Binary names

Release assets use the convention `rescrobble-<platform>-<architecture>`,
with `.exe` added for Windows. The MVP publishes these four binaries:

| Platform | Release asset | Example invocation |
| --- | --- | --- |
| Windows | `rescrobble-windows-amd64.exe` | `.\rescrobble-windows-amd64.exe --help` |
| Intel macOS | `rescrobble-darwin-amd64` | `./rescrobble-darwin-amd64 --help` |
| Apple Silicon macOS | `rescrobble-darwin-arm64` | `./rescrobble-darwin-arm64 --help` |
| Linux | `rescrobble-linux-amd64` | `./rescrobble-linux-amd64 --help` |

On macOS or Linux, grant execute permission after downloading if necessary:

```sh
chmod +x rescrobble-darwin-arm64
./rescrobble-darwin-arm64 --help
```

## Verify release downloads

Each release includes a `SHA256SUMS` manifest generated from exactly the
four binaries listed above. Before running a downloaded binary, download
`SHA256SUMS` into the same directory and verify the matching asset.

On macOS or Linux, run:

```sh
sha256sum --ignore-missing -c SHA256SUMS
```

If `sha256sum` does not support `--ignore-missing`, run
`sha256sum <asset>` and compare the digest with the matching line in
`SHA256SUMS`. In PowerShell, run:

```powershell
(Get-FileHash .\rescrobble-windows-amd64.exe -Algorithm SHA256).Hash.ToLower()
```

Compare the result with the `rescrobble-windows-amd64.exe` entry. Do not run a
binary if its checksum does not match.

GitHub CLI can also verify the signed build provenance for the exact downloaded
asset. Run the command matching the asset:

```sh
gh attestation verify ./rescrobble-windows-amd64.exe --repo wesback/scrobble-backfill
gh attestation verify ./rescrobble-darwin-amd64 --repo wesback/scrobble-backfill
gh attestation verify ./rescrobble-darwin-arm64 --repo wesback/scrobble-backfill
gh attestation verify ./rescrobble-linux-amd64 --repo wesback/scrobble-backfill
```

Do not run a binary if attestation verification fails.

## Publishing

Maintainers publish a release from a clean, up-to-date `main` checkout by
running `scripts/tag-release.sh v1.0.0`. The helper validates the version,
repository state, existing tags, and Go tests before creating and pushing the
annotated tag. Do not use `git push --tags` to publish a release.

GitHub Actions builds the four binaries on their target runners,
validates `--help` and `--version`, injects the tag as the binary version,
generates `SHA256SUMS`, attests each binary's build provenance, and publishes
the five named assets with generated release notes.
