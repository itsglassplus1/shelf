# shelf

## About
shelf is a lightweight, read-only comic and manga server for Mihon and Suwayomi,
accessed through the Komga extension.

shelf is for files on disk that no longer belong to a source: a finished
series moved out of your downloads, a series whose source has shut down, or a
collection you built yourself. Keep them in your own media folders, in
whatever layout you already use; you just name the folders that hold series. shelf reads titles, authors, chapter
numbers and covers from `ComicInfo.xml`, EPUB metadata and cover images, and
it never writes to your files.

## Scope

shelf is deliberately small. It implements only the part of Komga's API that
the extension uses to browse, show metadata and covers, and read. Everything
else, such as reading progress, is left to Mihon and Suwayomi (see
[Not supported](#not-supported)).

It also costs little to run:

- The image holds one static binary and nothing else. There's no database:
  the library index is built from the folders and kept in memory.
- Pages are sent as stored, without resizing or converting. ZIP, tar and EPUB
  chapters are read in place; only RAR chapters are unpacked, into a cache
  with a size cap.
- After 10 minutes without requests, shelf stops checking for changes, so
  disks can spin down.

## Setup

1. Take one of the examples and set the library path and `SHELF_CATEGORIES`
   (see [Library](#library)):
   - [examples/compose.yaml](examples/compose.yaml): Docker Compose
   - [examples/shelf.container](examples/shelf.container): Podman Quadlet
2. Start it. The first start creates a login (see [Login](#login)).
3. In Mihon or Suwayomi, install the Komga extension and set:
   - **Address**: `http://<host>:25600`
   - **Username**, **Password**: from the login file
   - **Chapter title format**: `{title}`. The default puts the number and file
     size into every chapter name.

Container images: `ghcr.io/itsglassplus1/shelf`, for amd64 and arm64. `:latest` is the
newest release; `:1`, `:1.2` and `:1.2.3` follow a release line.

## Configuration

| Variable | Default | |
|---|---|---|
| `SHELF_CATEGORIES` | (required) | Category folders in the library, separated by `:`, e.g. `comics/marvel:manga`. Each is a library in the extension. |
| `SHELF_CACHE_MAX_MB` | `2048` | Size cap for unpacked RAR chapters. A RAR chapter larger than this can't be read. |
| `SHELF_LISTEN` | `:25600` | Listen address. |
| `SHELF_AUTH` | `on` | `off` serves without a login. Only switch when nothing else can reach shelf. |

## Login

The login is in `credentials` in the folder mounted at `/data`: user `admin`
and a random password at first. Edit it to change them; changes apply within
seconds. Delete it to get a new random password.

Wrong passwords are answered after a second and logged with the client's
address (e.g. for fail2ban). shelf speaks plain HTTP: outside a trusted
network, put a reverse proxy with TLS in front. `/healthz` answers without a
login, for health checks.

## Library

The library is the folder mounted at `/library`. Inside it, a *category* is a
folder whose sub-folders are series. `SHELF_CATEGORIES` lists them, relative
to the library and separated by `:`. Each one becomes a library in the
extension. For example:

```
/path/to/library
├── comics
│   ├── marvel                  category "comics/marvel"
│   │   ├── Daredevil (2019)
│   │   │   ├── cover.jpg
│   │   │   ├── Daredevil 001.cbz
│   │   │   └── Daredevil 002.cbz
│   │   └── Ms. Marvel (2014)
│   └── dc                      category "comics/dc"
│       └── Batman (2016)
├── manga                       category "manga"
│   ├── Berserk
│   │   ├── ComicInfo.xml
│   │   ├── Vol.01 Ch.001.cbz
│   │   └── Vol.01 Ch.002/
│   │       ├── 001.jpg
│   │       └── 002.jpg
│   └── Frieren                 moved here from Suwayomi's downloads
│       ├── Chapter 1.cbz
│       └── Chapter 2.cbz
└── scans                       not listed: ignored
```

```
SHELF_CATEGORIES: "comics/marvel:comics/dc:manga"
```

- List the folder that directly holds the series. Listing `comics` here
  would show `marvel` and `dc` as series, with every comic in them as an empty
  chapter.
- Categories can't be nested: listing `comics` and `comics/marvel` together
  is an error.
- A series downloaded with Mihon or Suwayomi can be moved over as it is: move
  its folder into a category, like `Frieren` above. Both apps write
  `ComicInfo.xml` into their downloads, so the metadata comes along.
- Categories may contain spaces. In a Quadlet file, quote the whole setting:
  `Environment="SHELF_CATEGORIES=manga:light novels"`.

Inside a series:

```
<series>/
    cover.jpg           optional
    ComicInfo.xml       optional
    Chapter 01.cbz      a chapter per archive...
    Chapter 02/         ...or per folder of images
```

- Every archive and every sub-folder is a chapter.
- Chapter formats: `.cbz`/`.zip`, `.cbr`/`.rar`, `.cbt`/`.tar` (uncompressed),
  `.epub`, and folders of images.
- Page images: JPEG, PNG, GIF, WebP, JPEG XL, AVIF or HEIF. Files without an
  extension or with an unknown one are recognized by their contents.
- Hidden files and NAS folders (`@eaDir`, `#recycle`, `@Recycle`) are ignored.
- Changes appear within seconds while shelf is in use.
- Series folder names must be unique across categories.
- If two series share a name, a category is missing, or a series folder can't
  be read, shelf won't start. If it happens while shelf is running, it keeps
  serving the library as it was until the problem is fixed. The log says
  which.

## Metadata and covers

- **Series**: from `ComicInfo.xml` in the series folder, else from the newest
  chapter's, copied to `series/<series>/` in the data folder.
- **Chapters**: from each chapter's `ComicInfo.xml` (EPUB: its own metadata),
  else the file or folder name.
- **Chapter numbers**: from ComicInfo `Number`, else recognized from the names
  as Mihon does (`Vol.2 Ch.12.5`, `#003`, extras).
- **Covers**: `cover.*` in the series folder, else page 1 of the first
  chapter, copied to `series/<series>/` in the data folder.

Copies are made once. To change one, edit or replace it, or delete it to have
it made again.

## Not supported

- Reading progress: the extension's Unread, In progress and Read filters do
  nothing, and Mihon's Komga tracker can't sync.
- Collections, read lists, book search.
- 7z/cb7 archives, compressed tars.
- Editing metadata or uploading through the extension.

## License

shelf is licensed under the [Apache License 2.0](LICENSE). Its chapter number
recognition is ported from [Mihon](https://github.com/mihonapp/mihon); see
[NOTICE](NOTICE) for that and for the libraries built into the binary.
