# Kotodex

Follow manga on MangaDex and have new chapters downloaded to your own server, then read them in [Kotatsu](https://github.com/KotatsuApp/Kotatsu) on your phone. Kotodex is a small Go server with a web page for following manga, plus a plugin app that adds the server as a source in Kotatsu.

<p>
  <img width="32%" alt="Library of followed manga" src="screenshots/web-library.png" />
  <img width="32%" alt="Manga page with its chapters and download languages" src="screenshots/web-manga.png" />
  <img width="32%" alt="Downloads page with the queue and history" src="screenshots/web-downloads.png" />
</p>

## Features

- Follow manga from a MangaDex search or a pasted MangaDex link, and Kotodex downloads new chapters as they come out.
- Link MangaFire titles to existing manga, connect JSON chapter feeds, or upload chapters as an administrator. Chapters from every source share the same reader entry.
- Link a MangaDex account to follow everything you already follow there.
- Read downloaded chapters in Kotatsu, with its favorites, history, and new chapter notifications working like any other source.
- Give each person their own account with their own follows, and see or sign out the devices on each account.
- Pick which languages to download for each manga you follow, and download a manga's whole back catalog in one language from its page.
- See what everyone on the server is reading, with each person's progress, lists, and a year of reading time, once Kotatsu syncs with Kotodex.

Kotodex only downloads chapters that come out after you follow a manga. To grab the older ones, pick a language on the manga's page and use "Download all", e.g. "Download all English chapters". When two groups upload the same chapter, Kotodex downloads the first upload and lists the other one so you can download it by hand.

Kotatsu only shows chapters that are already on the server. Kotatsu shows queued chapters once Kotodex finishes downloading them.

## Usage

Start the server and sign in at `http://<server>:8080` as `admin`. Search MangaDex from the box at the top and press Follow on anything you want. Kotodex checks followed manga every hour by default. The Downloads page shows what's queued and lets you start a check right away.

To read on your phone, install the plugin APK, open it, and sign in with the server address and your Kotodex username and password. Kotodex then shows up pinned at the top of Explore in Kotatsu.

- If you want to see only your own follows in Kotatsu instead of everyone's, turn on "Only manga I follow" in the plugin app.
- Futon works the same as Kotatsu. If you use another Kotatsu fork, it gets an error the first time it opens Kotodex. Open the plugin app and press Allow next to it. Kotodex only answers apps you allowed, plus Kotatsu and Futon.
- Changing your password signs out every device, including Kotatsu. Sign in again in the plugin app afterwards.
- Kotodex signs out devices that go unused for 90 days. If you haven't opened Kotodex in Kotatsu for that long, sign in again in the plugin app.
- Linking a MangaDex account needs a personal API client. Create one on mangadex.org under Settings, API Clients, and wait for it to be approved.
- With a linked MangaDex account, Kotodex follows whatever you follow on MangaDex. Following or unfollowing something in Kotodex overrides that for one manga, and the reset button next to Follow on the manga's page goes back to matching MangaDex. Kotodex never changes your follows on MangaDex.
- MangaDex chapters from official publishers only link to the publisher's site. Kotodex can't download those and leaves them out.

To track reading, point Kotatsu's sync at Kotodex. In Kotatsu, open Settings, Services, Synchronization settings and set the server address. Then press Make sync password under Settings in Kotodex, open Synchronization in Kotatsu, and sign in with the email and sync password shown there. Your reading then shows up on your profile and under Activity.

- Kotodex becomes Kotatsu's sync server, so history and favorites sync between your phones through it. Kotatsu takes one sync server at a time.
- Pick who sees your reading under Settings: everything, only manga on Kotodex, or nothing. What you follow shows either way.
- Kotatsu only signs in with an email address. Kotodex reads the part before the @ as the username, so a username with spaces or symbols other than `._%+-` can't sign in to sync.
- Reading done offline shows up as one entry once the phone syncs again.
- Sync takes the sync password only, not your Kotodex password. Kotodex shows it once. To set up another phone, make a new one. Phones already signed in stay signed in.

Set your usual languages under Settings. Every manga you follow uses those unless you pick languages for it on the manga's page. The reset button there goes back to your usual ones. When people follow the same manga in different languages, Kotodex downloads every language someone wants and keeps one copy of each chapter.

Admins can add users and change how often to check under Settings.

If you forget the admin password, set a new one from the command line:

```bash
kotodex user passwd admin new-password
# or with Docker
docker compose exec kotodex /kotodex user passwd admin new-password
```

## Additional chapter sources

Open a manga page and expand **Manage sources**. Any signed-in reader can connect a source;
only administrators can disconnect a shared source or upload chapter files.
Choose **MangaFire**, paste its HTTPS title URL, and press **Connect source**. For example:
`https://mangafire.to/title/72xyj-kyou-mo-veranda-dee`.
Kotodex checks that title alongside MangaDex, including when the MangaDex chapter
check fails. Match the series yourself when connecting it; Kotodex does not guess
identity from similar titles. The catalog and manga IDs still come from MangaDex.

The first check lists the source's existing chapters without downloading them.
The chapter table starts with **MangaDex** selected. Open **Sources** beside the
language filter, check **MangaFire**, and press **Apply** to add its releases.
Uncheck MangaDex to show only the alternative. Language counts follow
the selected sources. Unchecking every source shows no chapters and queues nothing.
The filters are carried in the page URL and preserved when downloading.

**Download all** considers only the currently visible sources and languages.
The default priority is MangaDex, then MangaFire, then imported chapters. Use
the up/down buttons in **Sources** to set the full order, then press **Apply**.
Checkboxes choose visibility independently of the order. The ranking is preserved
in the page URL, language changes, and bulk downloads; it never enables a hidden source or replaces an existing download.
Within a source, the earliest release wins, with a stable ID as the tie-breaker.
Individual chapter download buttons still let you deliberately pick an alternate.

Kotodex gives each chapter a persistent numeric reader ID and associates its
source copies with that identity. Only the downloaded **Primary** copy is sent
to Kotatsu, producing one chapter list per language. Use **Make primary** on a
downloaded alternate to change the copy served under the same reader ID and
page reference. The source and scanlation group remain attached to each copy.
Downloading an alternate does not replace an available primary. Deleting the
primary falls back to another downloaded copy while retaining the reader ID.

Update both the server and Android plugin to retain chapter identity when
switching copies. The plugin uses the server's numeric ID directly; it falls
back to the previous conversion when connecting to an older server. On server
upgrade, the former numeric ID of the first downloaded copy is retained for
each chapter. Previously exposed alternate copies had separate identities;
their separate Kotatsu progress is not automatically combined. New identities
are allocated by Kotodex, independently of source IDs and volume metadata.

Already queued or downloaded chapter numbers in that language are skipped,
including equivalent numbers such as `01` and `1.0`. Connectors normalize labels
such as `Ch. 0012.50` to `12.5`, displayed as `Vol. 2 Ch. 12.5` when a volume is
known. Different nonempty volume numbers stay distinct; an unspecified volume
matches either. Numberless extras are kept separate. Identity comparisons use
exact decimal values, and queuing checks for duplicates atomically.

Future checks collect newly discovered, recent chapters across connected sources
before applying the default source priority. Older uploads remain available for
manual download. The page's source ranking does not change scheduled downloads.
Disconnecting a source stops polling it and keeps its chapter records and local files.

Administrators can also upload one chapter as a CBZ or ZIP, with chapter/volume,
language, title, group, and optional attribution URL. Numberless chapters are
supported. Images sort naturally by archive path and are stored without re-encoding.
Limits are 256 MiB compressed, 512 MiB expanded, 32 MiB per page, and 1000 pages;
JPEG, PNG, GIF, and WebP are supported. By default, uploads of an already queued
or downloaded chapter are refused; **Allow an alternate edition** explicitly keeps
another version. Deleting an upload removes its record because there is no remote
copy to retry. Reverse proxies must allow request bodies large enough for uploads.

Downloaded MangaFire and feed pages preserve the source's bytes. Kotodex cannot
promise that another source has the same resolution, edition, translation, or
scanlation as MangaDex. The Data saver setting applies to MangaDex only. Source
links remain visible in the chapter table. MangaFire support adapts the maintained
[Kotatsu-Redo parser](https://github.com/Kotatsu-Redo/kotatsu-parsers-redo) at the
revision documented in [its notice](server/third_party/kotatsu-parsers-redo/NOTICE.md).

### JSON chapter feeds and connectors

External adapters can expose an HTTP(S) JSON feed. Administrators can connect one
under **Manage sources → Advanced adapters**. This technical format is omitted
from the reader source picker; actual imported releases appear as **Imported chapters**. Feeds are newest first, page URLs are in reading order,
`id` is stable within the feed, `published` is Unix seconds (optional), and numbers
are decimal strings. An example:

```json
{
  "chapters": [
    {
      "id": "release-123",
      "number": "12.5",
      "volume": "2",
      "lang": "en",
      "title": "Chapter title",
      "group": "Scanlation group",
      "published": 1792400000,
      "url": "https://source.example/chapter/123",
      "pages": ["https://images.example/123/001.png"]
    }
  ]
}
```

The feed can contain at most 10000 chapters and 8 MiB of JSON. Feed/page requests
can reach public addresses only, including after DNS resolution and redirects.
They receive no Kotodex credentials, browser cookies, or personalized user-agent.
URLs with embedded credentials are refused. Standard Go HTTP user-agents are used
for MangaFire and JSON feeds; the existing MangaDex client retains its own agent.
Adapters requiring authentication or browser challenges need a separate integration.

Native connectors implement `ChapterSource` in `server/sources.go` and register
in `newDownloader`. Implement `NormalizeReference` to validate/canonicalize a
linked title URL and appear automatically in the source picker. Connectors provide
chapter discovery, page resolution, and
fetching so source-specific request signing and rate limits stay out of the worker.
Keep remote identities separate from the canonical manga ID. The shared worker
handles downloads, retries, cancellation, progress, and local page serving.

## Requirements

- Somewhere to run the server (Docker, or any Linux, macOS, or Windows machine)
- Kotatsu on Android 8.0 or newer for the plugin

## Installation

### With Docker

Edit `KOTODEX_ADMIN_PASSWORD` in `compose.yaml`, then:

```bash
docker compose up -d
```

Downloads and the database end up in the `kotodex-data` volume. To build the image yourself instead, replace the `image:` line with `build: ./server`.

### With systemd

Grab `kotodex-<version>-linux-amd64` (or `-arm64`) from the latest release, or build the server (see below). Copy it to `/usr/local/bin/kotodex`, then:

```bash
sudo cp contrib/kotodex.service /etc/systemd/system/
sudo systemctl enable --now kotodex
journalctl -u kotodex
```

If `KOTODEX_ADMIN_PASSWORD` isn't set, Kotodex prints a generated admin password to the log on first start. Data goes in `/var/lib/kotodex`.

### Plugin

Grab the APK from the latest release and install it on the phone that has Kotatsu.

## Configuration

The server reads these environment variables:

| Variable | Default | |
|---|---|---|
| `KOTODEX_DATA_DIR` | `./data` | Database and downloaded chapters |
| `KOTODEX_LISTEN` | `:8080` | Address and port to listen on |
| `KOTODEX_ADMIN_PASSWORD` | random | Password for the `admin` user created on first start |
| `KOTODEX_SECRET` | `secret.key` | Server key, at least 32 characters. Use it to keep the key out of the data directory, e.g. with Docker secrets |

Kotodex keeps its key in `secret.key` in the data directory, apart from the database. The key signs image links and encrypts linked MangaDex accounts. A copy of the database alone can't be used for either. Back up `secret.key` somewhere other than your database backups. If you lose it, link MangaDex accounts again. Kotatsu picks up new image links the next time it opens each manga.

To put Kotodex behind a reverse proxy, forward the `Host` header and set `X-Forwarded-Proto`. The plugin works over plain HTTP on a home network too.

## How to build

Server (Go 1.26 or newer):

```bash
cd server
go build -o kotodex .
go test ./...
```

Plugin (JDK 17 or newer and the Android SDK):

```bash
cd plugin
./gradlew assembleRelease
```

The APK ends up in `plugin/app/build/outputs/apk/release/`. `./gradlew testDebugUnitTest` renders the plugin's screens to `plugin/app/build/screenshots/`.

## Network activity

The server connects to:

- `api.mangadex.org` and `auth.mangadex.org` for search, chapter lists, and account follows
- `uploads.mangadex.org` for covers
- MangaDex@Home image servers (`*.mangadex.network`) for pages, and `api.mangadex.network` to report how each page download went, which MangaDex requires
- If an admin turns on loading covers of manga from other sources under Settings, the sites those covers are on, as synced from people's Kotatsu

The plugin only connects to your Kotodex server.
