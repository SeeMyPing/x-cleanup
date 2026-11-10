# x-cleanup

Delete your tweets and remove your likes on X, using the official data archive as the source of truth.

- **Tweets and likes are processed in parallel.** They use independent API quotas.
- **It never wastes a call.** Pacing follows the `x-rate-limit-*` response headers, so the tool waits for the quota window to reset instead of hitting a `429`.
- **It runs as a dry run by default.** Nothing is deleted until you pass `-go`.
- **You can resume it.** Stop it at any time (Ctrl+C, crash, reboot) and run the same command again. It picks up where it left off.
- **It has no dependencies.** It's a single static binary built from the Go standard library only, and it signs OAuth 1.0a requests itself.

> [!WARNING]
> Deletions are **permanent**. Keep a copy of your archive before running with `-go`.

## How it works

X's API can't list all of your old tweets and likes, but your archive does. x-cleanup reads the tweet and like IDs from the archive. Then it calls:

| What   | Endpoint                                   |
| ------ | ------------------------------------------ |
| Tweets | `DELETE /2/tweets/:id`                     |
| Likes  | `DELETE /2/users/:your_id/likes/:tweet_id` |

Every successful ID is appended to a `done_*.txt` file, so it's never sent twice.

Only what is **in the archive** is processed. Tweets posted or likes added after you requested the archive are left untouched.

## Prerequisites

### 1. Download your X archive

On X, go to **Settings → Your account → Download an archive of your data**. It can take a day or more to be ready. Unzip it once you have it. The folder you'll pass to x-cleanup is the one that contains `data/`:

```
twitter-archive/
└── data/
    ├── account.js
    ├── tweets.js
    ├── tweets-part1.js   (large accounts are split into parts)
    └── like.js
```

### 2. Create an X developer app

1. Open [console.x.com](https://console.x.com) and create an app.
2. Set the app permissions to **Read and write**. Do this **before** generating the Access Token. A token generated earlier stays read-only, and every call fails with `403`.
3. Under **Keys and tokens**, generate the API Key/Secret and the Access Token/Secret.
4. Make sure your account has API credits. The X API is paid per use, and x-cleanup stops cleanly if credits run out.

### 3. Export your credentials

```sh
export X_API_KEY="..."
export X_API_SECRET="..."
export X_ACCESS_TOKEN="..."
export X_ACCESS_SECRET="..."
```

You only need the credentials when running with `-go`. A dry run doesn't call the API.

## Installation

### Prebuilt binaries

Download the binary for your platform from the [Releases](https://github.com/SeeMyPing/x-cleanup/releases) page:

| Platform              | File                          |
| --------------------- | ----------------------------- |
| Windows (64-bit)      | `x-cleanup-windows-amd64.exe` |
| Linux (64-bit)        | `x-cleanup-linux-amd64`       |
| Linux (ARM64)         | `x-cleanup-linux-arm64`       |
| macOS (Apple Silicon) | `x-cleanup-darwin-arm64`      |

Each release includes a `SHA256SUMS` file for checking the downloads.

On Linux or macOS, make the file executable:

```sh
chmod +x x-cleanup-*
```

The macOS binary isn't signed. If Gatekeeper blocks it, run:

```sh
xattr -d com.apple.quarantine x-cleanup-darwin-arm64
```

### Docker

```sh
docker pull ghcr.io/seemyping/x-cleanup:latest
```

The image is available for `linux/amd64` and `linux/arm64`.

### From source

You need Go 1.24 or later.

```sh
go build -o x-cleanup .
```

## Usage

```
x-cleanup [options] <archive-dir>
```

Options must come **before** the archive directory.

| Option                | Default | Description                                        |
| --------------------- | ------- | -------------------------------------------------- |
| `-go`                 | off     | Actually delete. Without it, the run is a dry run. |
| `-only tweets\|likes` | both    | Process only tweets or only likes.                 |
| `-limit N`            | `0`     | Process at most N items per type (`0` = no limit). |
| `-state-dir DIR`      | `.`     | Where to write `done_*.txt` and `errors.log`.      |

### Recommended workflow

**1. Dry run.** See what would be done, how much it would cost and how long it would take:

```sh
./x-cleanup ./twitter-archive
```

```
10:02:11 Dry run: nothing will be deleted. Add -go to execute.
10:02:11 [tweets] 4210 in archive, 0 already processed, 4210 to do — ≈ $42.10, ≈ 21h3m0s
10:02:11 [likes] 9830 in archive, 0 already processed, 9830 to do — ≈ $98.30, ≈ 49h9m0s
10:02:11 Total: ≈ $140.40 (pricing for likes to be confirmed), ≈ 49h9m0s in parallel
```

**2. Small live test.** Delete a few items to check your credentials and permissions:

```sh
./x-cleanup -go -limit 3 ./twitter-archive
```

**3. Full run.**

```sh
./x-cleanup -go ./twitter-archive
```

Large accounts can take a long time to process, so run it somewhere it can keep going (`tmux`, `screen`, a server). If it stops, run the same command again to resume.

### With Docker

Mount a working directory that contains the archive. The state files are written there too:

```sh
docker run --rm -it \
  -v "$PWD:/work" \
  -e X_API_KEY -e X_API_SECRET -e X_ACCESS_TOKEN -e X_ACCESS_SECRET \
  ghcr.io/seemyping/x-cleanup -go ./twitter-archive
```

The container runs as a non-root user. On Linux, add `--user "$(id -u):$(id -g)"` so it can write to the mounted directory.

## Cost and duration

The estimates in the dry run use these numbers:

- **Price:** about **$0.01 per deletion**. The like rate hasn't been confirmed yet, so check the current X API pricing.
- **Rate:** about **50 deletions per 15-minute window**, per endpoint. Tweets and likes run in parallel, so the total time is that of the longer of the two.

These are estimates. The actual pace follows the rate-limit headers X returns.

## Output files

All files are written to `-state-dir`, which defaults to the current directory.

| File              | Content                                                                  |
| ----------------- | ------------------------------------------------------------------------ |
| `done_tweets.txt` | IDs of tweets deleted or already gone, one per line.                     |
| `done_likes.txt`  | IDs of tweets un-liked or already gone, one per line.                    |
| `errors.log`      | Failed items, tab-separated: time, type, ID, HTTP status, response body. |

Failed items aren't added to `done_*.txt`, so the next run tries them again. To start over from scratch, delete the `done_*.txt` files.

## Error handling

| Situation                              | Behaviour                                                                                                 |
| -------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `404 Not Found`                        | Already deleted or unavailable. Counted as done.                                                          |
| `429 Too Many Requests`                | Waits until the quota window resets, then continues.                                                      |
| `5xx` or network error                 | Retries up to 5 times with increasing back-off.                                                           |
| `401 Unauthorized`                     | **Stops**: invalid keys. Check the 4 environment variables.                                               |
| `403 Forbidden`                        | **Stops**: the app isn't *Read and write*, or the token predates the change. Regenerate the Access Token. |
| `402` or a response mentioning credits | **Stops**: API credits are exhausted. Top up on console.x.com and run again.                              |
| Any other error                        | Logged to `errors.log`. Processing continues.                                                             |

When a fatal error happens in one worker, both workers stop immediately.

### Exit codes

| Code  | Meaning                                               |
| ----- | ----------------------------------------------------- |
| `0`   | Finished (some items may be listed in `errors.log`).  |
| `1`   | Fatal error (credentials, permissions, credits, …).   |
| `2`   | Invalid command line.                                 |
| `130` | Interrupted (Ctrl+C or SIGTERM). Run again to resume. |

## Development

```sh
go vet ./...
go test -race ./...
```

The tests check the OAuth signature against the official reference example. They also run end-to-end against a fake X server, covering parallelism, rate limiting, resuming and fatal errors.

To point the tool at another API base URL (for example a local mock), set `X_API_BASE`:

```sh
X_API_BASE=http://localhost:8080/2 ./x-cleanup -go ./twitter-archive
```

### CI

- [`.github/workflows/build.yml`](.github/workflows/build.yml) runs the tests, then builds the four binaries. When you push a `v*` tag, it publishes a GitHub release.
- [`.github/workflows/docker.yml`](.github/workflows/docker.yml) builds the multi-arch image and pushes it to GHCR on `main` and on `v*` tags, with provenance and SBOM attestations.
- [`.github/workflows/pr.yml`](.github/workflows/pr.yml) runs the tests on pull requests. A maintainer has to approve each run first (`pr-tests` environment with required reviewers). It has no secrets and a read-only token.

- [`.github/workflows/release.yml`](.github/workflows/release.yml) creates a release on demand from `main`. It works out the next version, runs the tests, creates the tag, then starts *Build* and *Docker* on that tag.

To publish a release, run the **Release** workflow from the Actions tab, or from the command line:

```sh
gh workflow run release.yml -f bump=patch     # or minor / major
gh workflow run release.yml -f version=v2.0.0 # explicit version
```

Pushing a tag by hand (`git tag v1.2.3 && git push origin v1.2.3`) also still works.
