# Rescrobble releases

The supported MVP distribution channel is **GitHub Releases**. Download the
binary for your platform from the release assets, make it executable where
required, and invoke it from a command line.

The MVP does not advertise or support installation through Homebrew, Scoop, or
Docker. Those distribution channels are intentionally deferred.

## Binary names

Release assets use the convention `rescrobble-<platform>-<architecture>`,
with `.exe` added for Windows. The MVP publishes these three amd64 binaries:

| Platform | Release asset | Example invocation |
| --- | --- | --- |
| Windows | `rescrobble-windows-amd64.exe` | `.\rescrobble-windows-amd64.exe --help` |
| macOS | `rescrobble-darwin-amd64` | `./rescrobble-darwin-amd64 --help` |
| Linux | `rescrobble-linux-amd64` | `./rescrobble-linux-amd64 --help` |

On macOS or Linux, grant execute permission after downloading if necessary:

```sh
chmod +x rescrobble-darwin-amd64
./rescrobble-darwin-amd64 --help
```

## Publishing

Maintainers publish a release by pushing a version tag such as `v1.0.0`.
GitHub Actions cross-compiles the three binaries, injects that tag as the
binary version, and uploads each named asset to the matching GitHub Release.
