# shelf

A read-only comic and manga server for a library on disk. Mihon and Suwayomi
read it through their Komga extension, with chapters, metadata and covers as
Mihon's local source shows them. Shelf never writes to the library.

## Setup

1. Take one of the examples and set the library path and `SHELF_CATEGORIES`:
   - [examples/compose.yaml](examples/compose.yaml): Docker Compose
   - [examples/shelf.container](examples/shelf.container): Podman Quadlet (systemd)
2. Start it. The first start creates a login; the example shows where to read it.
3. In Mihon or Suwayomi, install the Komga extension and set:
   - **Address**: `http://<host>:25600`
   - **Username**, **Password**: from the login file
   - **Chapter title format**: `{title}`. The default puts the number and file
     size into every chapter name.

Images: `ghcr.io/itsglassplus1/shelf`, for amd64 and arm64. `:latest` is the
newest release; `:1`, `:1.2` and `:1.2.3` follow a release line.

## Configuration

| Variable | Default | |
|---|---|---|
| `SHELF_CATEGORIES` | (required) | Category folders in the library, separated by `:`, e.g. `comics/marvel:manga`. Each is a library in the extension. |
| `SHELF_ROOT` | `/library` | The library, mounted read only. |
| `SHELF_DATA_DIR` | `/data` | Shelf's own files: login, unpacked RAR chapters, cover and ComicInfo.xml copies. |
| `SHELF_CACHE_MAX_MB` | `1024` | Size cap for unpacked RAR chapters. A RAR chapter larger than this can't be read. |
| `SHELF_LISTEN` | `:25600` | Listen address. |
| `SHELF_AUTH` | `on` | `off` serves without a login. Only when nothing else can reach shelf. |
| `SHELF_PPROF` | (off) | Address for Go's profiling endpoints, e.g. `127.0.0.1:6060`. No login, separate from the API; for measuring performance only. |

## Login

The login is in `credentials` in the data folder: user `admin` and a random
password at first. Edit it to change them; changes apply within seconds.
Delete it to get a new random password.

Wrong passwords are answered after a second and logged with the client's
address (e.g. for fail2ban). Shelf speaks plain HTTP: outside a trusted
network, put a reverse proxy with TLS in front. `/healthz` answers without a
login, for health checks.

## Library

```
<library>/<category>/<series>/
    cover.jpg           optional
    ComicInfo.xml       optional
    Chapter 01.cbz      a chapter per archive...
    Chapter 02/         ...or per folder of images
```

- Every folder in a category is a series; in a series, every archive and
  every sub-folder is a chapter.
- Chapter formats: `.cbz`/`.zip`, `.cbr`/`.rar`, `.cbt`/`.tar` (uncompressed),
  `.epub`, and folders of images. Pages are JPEG, PNG, GIF, WebP, JPEG XL, AVIF
  or HEIF.
- Hidden files and NAS folders (`@eaDir`, `#recycle`, `@Recycle`) are ignored.
- Changes appear within seconds. After 10 minutes without requests shelf stops
  checking, so disks can spin down.
- Shelf doesn't start, and later keeps the previous state, while a category
  is missing, a series folder can't be read, or two categories contain a
  series with the same folder name. The log says which.

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
