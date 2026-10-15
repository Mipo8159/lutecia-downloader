# Lutecia downloader

Double-click it, and it gets the newest data out of APEX and saves it as CSV files ready for the Bitrix24 import:

1. asks the Lutecia sync server to pull the latest data from APEX now, and waits for it (10–30 seconds);
2. downloads every new data folder into **`Bitrix import/`** in this folder;
3. opens that folder and says in which order to import.

Only new data is downloaded, never the same folder twice.

## Setup (once per computer)

1. Get this folder onto the computer: `git clone <repo url>`, or on GitHub **Code → Download ZIP** and unzip it.
2. Copy `.env.example` to **`.env`** (same folder) and fill in the token:
   ```
   LUTECIA_SERVER=http://<server address>:3000
   LUTECIA_TOKEN=<the server's SYNC_API_TOKEN>
   ```
   On Windows, Notepad can do it: open `.env.example`, fill in the token, then **File → Save As**, set the file name
   to `".env"` with the quotes and "Save as type" to *All files*.

That is all. `.env` stays on that computer; it is never uploaded (see `.gitignore`).

## Use

| Computer | Double-click |
|---|---|
| Windows | `Lutecia Download.exe` |
| macOS, Linux | `Lutecia Download.command` |

**First time only:**
- **Windows** may show "Windows protected your PC": click **More info → Run anyway**.
- **macOS** may say it can't verify the developer: open **System Settings → Privacy & Security** and click
  **Open Anyway** next to "Lutecia Download.command".

You get neither warning if the folder was put on the computer with `git clone`.

**Then, in Bitrix24:** import each new folder in `Bitrix import/` **once, oldest first**. Inside each folder,
`IMPORT_ORDER.txt` gives the order of the files. When a folder is imported, you may delete it: it is not
downloaded again. Do not edit or delete `Bitrix import/downloaded.txt`: it records what was already downloaded.
Without it everything is downloaded again, and importing deals twice duplicates them in Bitrix.

## Messages

| It says | Meaning |
|---|---|
| `no .env next to this program` | Setup step 2 is missing |
| `cannot reach the server` | The server is off, or this computer is not on its network / VPN |
| `the server rejected LUTECIA_TOKEN` | Wrong token in `.env` |
| `no fresh data this time: …` | APEX couldn't be read just now. You still get everything the server had; try again later |
| `Nothing new` | All downloaded already. The server also pulls by itself every 3 hours |

`--no-pull` (from a terminal) skips step 1 and only downloads.

## For developers

```
Lutecia Download.exe       Windows binary (built)
Lutecia Download.command   macOS/Linux launcher: picks bin/lutecia-download-<os>-<arch>
bin/                       macOS (Apple Silicon, Intel) and Linux (x86-64, ARM) binaries (built)
src/                       Go source and tests
build.sh                   tests + rebuilds all binaries; needs only Docker
```

After changing `src/`, run `./build.sh` and commit the rebuilt binaries with the source. Builds are
reproducible, so unchanged source gives byte-identical binaries and no git change.

The server side is the `lutecia-sync` NestJS service: `POST /sync/runs` (start a pull), `GET /sync/runs/:id`
(poll it), `GET /bundles` (committed bundles, oldest first) and `GET /bundles/:id/files/:name`, all with
`Authorization: Bearer <SYNC_API_TOKEN>`. A bundle holds only rows that are new or changed since the previous one,
so each must be imported exactly once, in order; that is why the downloader keeps `downloaded.txt`.
