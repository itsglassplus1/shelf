# shelf

A read-only comic and manga server for a local library. It answers the part
of Komga's API that the Komga extension uses, so Mihon or Suwayomi can browse
and read the library through that extension. Chapters, metadata and covers
behave like Mihon's local source.

It never writes to the library. Generated files (login, unpacked RAR
chapters, cover and ComicInfo.xml copies) go to a separate data folder.

## Run

```sh
podman run -d --name shelf \
  -p 25600:25600 \
  -v /path/to/library:/library:ro \
  -v shelf-data:/data \
  -e SHELF_CATEGORIES=comics:manga \
  ghcr.io/itsglassplus1/shelf:latest
```

Images are built for amd64 and arm64 and published for version tags: `v1.2.3`
publishes `:1.2.3`, `:1.2`, `:1` and `:latest`. `:main` is a development build,
published only when the workflow is run by hand.

To build locally: `podman build -t localhost/shelf .` (runs the tests first).

On SELinux hosts (Fedora, RHEL), add `z` to bind mounts:
`-v /path/to/library:/library:ro,z`.

### User

The image runs shelf as uid and gid 1000, usually the host's first user.
`--user` sets another, e.g. `--user 1001:1001`.

- The library must be readable by that user (world-readable files are).
- A new named volume at `/data` gets that owner automatically.
- A volume used with an image from before this change belongs to root.
  Shelf logs an error naming the folder; change the owner once:
  `podman run --rm -v shelf-data:/data docker.io/library/alpine chown -R 1000:1000 /data`
- A bind-mounted data folder must be writable by that user. With rootless
  Podman, add `--userns=keep-id`: the container's 1000 is then you on the
  host, so shelf's files are yours and it can read what you can.

## Configuration

| Variable | Default | |
|---|---|---|
| `SHELF_CATEGORIES` | (required) | Category folders inside `SHELF_ROOT`, separated by `:`, e.g. `comics/marvel:manga`. Each becomes a Komga library. |
| `SHELF_ROOT` | `/library` | Library root. Read only. |
| `SHELF_DATA_DIR` | `/data` | Writable folder for shelf's own files. |
| `SHELF_CACHE_MAX_MB` | `1024` | Soft size cap for unpacked RAR chapters. A RAR chapter that unpacks to more can't be read. |
| `SHELF_LISTEN` | `:25600` | Listen address. |
| `SHELF_AUTH` | `on` | `off` serves without a login. |

Data folder:

```
credentials              login
cache/                   RAR chapters, unpacked on first read
series/<series folder>/  cover and ComicInfo.xml copies
```

## Login

On first start shelf creates `<data>/credentials` with the user `admin` and a
random password, and logs where it is. Edit the file to change them; changes
apply within seconds. Delete it to get a new random password.

```
username = admin
password = ...
```

Spaces around the username and password are ignored. A wrong username or
password is answered after a second and logged with the client's address
(at most once a minute per address), e.g. for fail2ban.

Shelf speaks plain HTTP, so the password and session cookie can be read on
the way. Outside a trusted network, put a reverse proxy with TLS in front.

`/healthz` needs no login. With `SHELF_AUTH=off` anyone who can reach the port
can read the library; use it only when nothing else can reach it, e.g.
Suwayomi in the same pod.

Shelf starts only if the library is valid: every category exists, none is
listed twice or nested in another, and no series folder name appears in two
categories. A later problem is logged and the previous library state is kept.

## Extension settings

In the Komga extension (Mihon or Suwayomi):

- **Address**: `http://<host>:25600`
- **Username**, **Password**: from `<data>/credentials`. API keys are not supported.
- **Chapter title format**: `{title}`. The default, `{number} - {title} ({size})`,
  puts the number and file size into every chapter name, which Mihon's local
  source doesn't.

## Library layout

```
<SHELF_ROOT>/<category>/<series>/
    cover.jpg               optional
    ComicInfo.xml           optional, series metadata
    Chapter 01.cbz          a chapter per archive...
    Chapter 02/             ...or per folder of images
```

- Every folder directly in a category is a series. Loose files in a category
  are ignored.
- In categories and series, names starting with `.` are ignored, as are NAS
  and filesystem folders: `@eaDir`, `#recycle`, `@Recycle`, `lost+found`,
  `$RECYCLE.BIN`, `System Volume Information`.
- In a series, every supported archive and every sub-folder is a chapter.
- Series and pages are in natural order (`Chapter 2` before `Chapter 10`).
  The extension orders chapters by number.
- Changes are picked up within seconds while shelf is in use. After 10 minutes
  without requests it stops checking, so disks can spin down; the next request
  checks first.

## Formats

| Format | Notes |
|---|---|
| `.cbz` `.zip` | |
| `.cbr` `.rar` | Unpacked into `cache/` on first read; least recently read evicted. Covers read only page 1. |
| `.cbt` `.tar` | Uncompressed only. Compressed tars are refused with a log message. |
| `.epub` | Pages are the images in reading order. Metadata from the EPUB, not ComicInfo.xml. |
| Folder | Images directly in the folder. |

Archive formats are detected by content, so misnamed files work. Pages are
JPEG, PNG, GIF, WebP, JPEG XL, AVIF or HEIF. A file is a page if its
extension says so or, failing that, if its content is one of these. Hidden
files and `__MACOSX/` are skipped.

## Metadata

### Series

From the first of:

1. `ComicInfo.xml` in the series folder.
2. `<data>/series/<series>/ComicInfo.xml`.
3. The newest chapter's `ComicInfo.xml`, copied to (2). For an EPUB, its
   author and description (not copied).

Delete the copy in (2) to have it made again. A file placed there by hand is
used the same way.

| ComicInfo | Shown as |
|---|---|
| `Series` | Title (else the folder name) |
| `Summary` | Description |
| `Writer` | Author |
| `Penciller`, `Inker`, `Colorist`, `Letterer`, `CoverArtist` | Artist |
| `Genre`, `Tags`, `ty:Categories` | Genres |
| `Publisher` | Publisher filter |
| `ty:PublishingStatusTachiyomi` | Status. `Licensed` shows as Unknown. |

### Chapters

| ComicInfo | EPUB | Shown as |
|---|---|---|
| `Title` | `dc:title` | Name (else the file or folder name) |
| `Number` | | Number (see below) |
| `Translator` | `dc:publisher`, else `dc:creator` | Scanlator |
| `Year`, `Month`, `Day` | `dc:date`, else `dcterms:modified` | Date (else the file's modification time) |

Chapters that would share a name are renamed after their file, so Suwayomi
keeps them apart. The log lists such renames.

### Chapter numbers

Recognized as in Mihon (`ChapterRecognition`): a ComicInfo `Number` if it is a
number, else from the file name (`Vol.2 Ch.12.5`, `#003`, `Part 2`, extras),
else from the chapter name, with gaps filled from neighbouring chapters.

Unlike Mihon's local source, file names are read together, so a number every
similar name shares (a group tag, a year) is ignored. Chapters without a
recognizable number get `-2` and are listed in the log.

### Covers

From the first of:

1. `cover.*` in the series folder: any or no extension, any case, if the file
   is an image.
2. `<data>/series/<series>/cover.*`.
3. Page 1 of the first chapter, saved as (2).

Delete the copy in (2) to have it made again.

## Not supported

- Reading progress. The extension's Unread, In progress and Read filters do
  nothing, and Mihon's Komga tracker (Settings > Tracking) can't sync.
- Collections, read lists and book search: empty results.
- 7z/cb7 archives and compressed tars.
- Changing metadata or uploading through the API.

## Development

```sh
go test ./...
```
