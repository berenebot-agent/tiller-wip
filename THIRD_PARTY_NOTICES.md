# Third-party notices

Tiller Router is distributed under the [GNU Affero General Public License,
version 3](LICENSE). The dependencies below are separate works; their own
licenses continue to apply. Versions are the direct and transitive Go modules
listed in this repository's `go.mod`.

## Go modules

| Module | Version | License | Upstream notice |
| --- | --- | --- | --- |
| `golang.org/x/crypto` | v0.55.0 | BSD-3-Clause | [LICENSE](https://github.com/golang/crypto/blob/master/LICENSE) |
| `modernc.org/sqlite` | v1.39.1 | BSD-3-Clause | [LICENSE](https://gitlab.com/cznic/sqlite/-/blob/master/LICENSE) |
| `github.com/dustin/go-humanize` | v1.0.1 | MIT | [LICENSE](https://github.com/dustin/go-humanize/blob/master/LICENSE) |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause | [LICENSE](https://github.com/google/uuid/blob/master/LICENSE) |
| `github.com/mattn/go-isatty` | v0.0.20 | MIT | [LICENSE](https://github.com/mattn/go-isatty/blob/master/LICENSE) |
| `github.com/ncruces/go-strftime` | v0.1.9 | MIT | [LICENSE](https://github.com/ncruces/go-strftime/blob/main/LICENSE) |
| `github.com/remyoudompheng/bigfft` | 24d4a6f8 | BSD-3-Clause | [LICENSE](https://github.com/remyoudompheng/bigfft/blob/master/LICENSE) |
| `golang.org/x/exp` | b7579e27df2b | BSD-3-Clause | [LICENSE](https://github.com/golang/exp/blob/master/LICENSE) |
| `golang.org/x/sys` | v0.47.0 | BSD-3-Clause | [LICENSE](https://github.com/golang/sys/blob/master/LICENSE) |
| `modernc.org/libc` | v1.66.10 | BSD-3-Clause | [LICENSE](https://gitlab.com/cznic/libc/-/blob/master/LICENSE) |
| `modernc.org/mathutil` | v1.7.1 | BSD-3-Clause | [LICENSE](https://gitlab.com/cznic/mathutil/-/blob/master/LICENSE) |
| `modernc.org/memory` | v1.11.0 | BSD-3-Clause | [LICENSE](https://gitlab.com/cznic/memory/-/blob/master/LICENSE) |

The modernc.org modules form the SQLite implementation's transitive runtime
dependency set. The Go toolchain may also download additional modules in the
module graph; `go.mod` and `go.sum` are authoritative for a particular
checkout. The container image includes the applicable dependency license texts
under `/licenses`, including embedded subcomponent notices from the modernc.org
modules. Notices and source are also available from each upstream repository.

## Test-only tooling and images

The browser test harness declares Playwright Test `1.55.0` in
`tests/browser/package.json` and `package-lock.json`. It is test-only and is
not shipped in the Tiller Router image; see the [Playwright license](https://github.com/microsoft/playwright/blob/main/LICENSE)
and the installed package notices when running the browser harness. The
compatibility and browser harnesses also use their declared test-container base
images, and the reverse-proxy smoke uses the official Nginx Alpine test image.
These test-only tools are not application runtime dependencies.

No provider SDK, JavaScript bundle, or vendored third-party source is included
in the application image. Review the upstream notices before redistributing a
modified build or a test environment.
