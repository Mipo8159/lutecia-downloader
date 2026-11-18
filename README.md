# Lutecia downloader

One double-click gets the newest Lutecia data, ready for the Bitrix24 import.

```
Windows/Lutecia.exe        for Windows
Mac/Lutecia.command        for Mac (Linux: run ./Lutecia.command in a terminal)
```

## Setup (once per computer)

1. Get this folder onto the computer: on GitHub **Code → Download ZIP**, then unzip it (or `git clone <repo url>`).
2. Open the folder for your computer (**Windows** or **Mac**) and double-click **Lutecia**.
3. The first time, a settings file opens (Notepad on Windows, TextEdit on Mac). Fill in the two lines Sweeft gives
   you (`LUTECIA_SERVER`, `LUTECIA_TOKEN`), **save**, close it, and double-click **Lutecia** again.

The settings are saved in `.env` next to the program; it is never uploaded to GitHub. Lutecia opens it again by
itself if something in it is wrong (for example a changed access key).

**First time only:**
- **Windows** may show "Windows protected your PC": click **More info → Run anyway**.
- **Mac** may say it can't check the program: open **System Settings → Privacy & Security**, scroll down, and click
  **Open Anyway** next to "Lutecia.command". If it asks for access to a folder, click **Allow**.

## Every time you need fresh data

Double-click **Lutecia**. It asks the server for the latest data (a timer shows it is working), says **DONE** with
the numbers, and opens a **new folder** inside **`Lutecia for Bitrix`** (next to the program), named after the date and
time of the download:

```
Lutecia for Bitrix/
  2026-11-16 14-05 (everything)/   the first download on this computer: all the data
  2026-11-17 09-30/                the next one: only what is new or changed since 2026-11-16 14-05
  2026-11-18 10-12/                ... since 2026-11-17 09-30
```

The server remembers when this office last downloaded, so each folder holds only what came after the folder before
it. **Import the folders oldest first, each one once, then delete it.** A folder holds:

- the files to import (`products.csv`, `offline_contacts.csv`, ...). If you open one in Excel, don't save it.
- **`tracking_events.csv`** (since 2026-11-19): the web shop's tracking events (which product or brand page was
  opened, when), exactly as the shop sends them, one row per page view. The first download has every event since
  2026-11-05; each later folder only the events since the folder before. Bitrix24 has no import for it: look at it
  in the `excel` folder (`tracking_events.xlsx`; very large downloads come in parts, `(part 1)`, `(part 2)`).
- **`excel`**: copies for looking at the data. Double-click them; Georgian text, columns and barcodes show correctly.
  Never import these into Bitrix. They hold customer data: delete them with the folder (if you delete only the
  import files, the next double-click removes the copies).
- **`test`** (only in the first, "everything" folder): a small part of it (about 10 deals, their buyers and their
  products, plus 20 recent tracking events) for trying an import. The test deals have names starting with `TEST`: delete them in Bitrix before
  importing the real files, or those deals are there twice. Temporary: may be dropped later.

Import the files of each folder into Bitrix24 **in this order**, then **delete the folder**:

1. `products.csv` (CRM → Products → Import)
2. `offline_contacts.csv` (CRM → Contacts → Import)
3. `online_contacts.csv` (CRM → Contacts → Import; duplicate control as for `offline_contacts.csv`, by Contact key,
   "merge": a web shop customer who is also a store customer has the same Contact key and name and only adds e-mail,
   address and the online columns)
4. `offline_deals.csv` (CRM → Deals → Import)
5. `online_deals.csv` (CRM → Deals → Import): web shop orders, **the same columns as `offline_deals.csv`**, so the same
   field mapping. Store is "lutecia.ge". The shop gives no order date: Sale date is the day the order was first
   downloaded (empty for orders older than the first download).

**In the Bitrix24 import choose encoding UTF-8 and separator comma.** The files are UTF-8 with a BOM (Georgian shows
correctly), but **never open and save them in Excel** before importing: Excel saves in another encoding and with `;`,
which breaks Georgian letters and turns phones like +995599123456 into numbers. To look at the data use the `excel`
folder (phones and barcodes stay text there).

Each double-click brings only what is new or changed since the last download, in its own folder; nothing new = no
folder ("Everything is up to date"). Forgot to import before double-clicking again? Nothing is lost: import the older
folder first, then the newer one (the closing message lists the folders still there). A new unzip in the same office
also gets only what is new, because the server knows the office. **Never delete a folder before it is imported**: its
data is not sent again (ask Sweeft if it happened).

Copies from before 2026-11-16 have one `production` folder that every download was merged into. It stays as it was:
the first double-click of the new program tells the server what it holds, and from then on new data arrives in dated
folders. Import `production` first if it is not imported yet.

`offline_deals.csv` holds the purchases of the last year (one deal per receipt, its products as rows). Bitrix import can
only add deals, never remove them, so **once a month** delete the old ones: CRM → Deals → list view → filter
"Sale date" before today minus one year → select all → Delete.

Columns with Bitrix's own names map by themselves. Create the others once in Bitrix as custom fields (product
properties for products) and map them at import:

- **Contacts:** Contact key, Loyalty card, Card discount %, Loyalty points, Card disabled, Home store, Customer since,
  Purchases, Total spend, First purchase, Last purchase, Average purchase, Purchases (last year), Spend (last year),
  Returns.
- **Deals:** Contact key, Sale date (a date field), Sale time, Loyalty card, Store, Seller, Payment, Items,
  Brands, Categories.
- **Products:** Brand, Color / line, Gender, Barcode, Sold by.
- **Online contacts** (same contact fields, plus): Name on website, Website account, Registered on website, Last login,
  Online orders, Online spend, Also a store customer.

**Contact key** is the customer's id: a deal's buyer is always the contact with the same Contact key. The import
links a deal to its contact by first and last name ("Contact"); when two contacts share a name that column is left
empty, so the deal is not attached to the wrong person, and Contact key still says who bought.
**Sale date** is the real date of the purchase. The file has no Close date: Bitrix fills its own (often the import day).

What the CRM can answer with this, for example: contacts whose Last purchase is over 6 months ago; deals whose Brands
contains DIOR in the last 3 months, by Store or Seller (their contacts are the DIOR buyers); the best customers by
Spend (last year).

## For developers

```
Windows/Lutecia.exe    Windows program (built)
Mac/Lutecia.command    macOS + Linux: a short script with the macOS (Apple Silicon, Intel) and Linux programs
                       packed after it; it starts the right one (built)
src/                   Go source and tests; src/env.example is the settings template built into the program
build.sh               tests + rebuilds both files; needs only Docker; reproducible (same source = same bytes)
```

One file for every system is impossible: Windows runs only `.exe` files, and a Mac won't open one.
After changing `src/`, run `./build.sh` and commit both files with the source.

Production servers need `https://` in `LUTECIA_SERVER`: the token goes with every request, so plain `http://`
is accepted only for a local or private address.

How it works: the server (`lutecia-heartbeat`) makes a bundle per pull with only the new or changed rows, and keeps a
**checkpoint per client IP**: the last bundle that client finished downloading. Each click: `POST /heartbeat/runs` (and
wait), `GET /downloads` (the committed bundles after this IP's checkpoint; all of them the first time),
`GET /bundles/:id/files/:name` for each, then `PUT /downloads/checkpoint {"bundle": <last id>}`, all with
`Authorization: Bearer <token>`. The bundles are merged into one file per entity in a new dated folder (the server's
`contacts.csv` and `deals.csv` are saved as `offline_contacts.csv` and `offline_deals.csv`): a newer record replaces
the one with the same key (products: External ID; contacts: Contact key; deals: Deal key, the deal's rows moved as a
whole and its `ID` renumbered, since every bundle numbers its deals from 1). The folder is built under a hidden
`.<name>.partial-*` name and renamed only when complete; the checkpoint moves only after that, so a cut download
neither skips nor repeats data.

The downloaded bundle ids are also listed next to the program (`.lutecia-downloaded-*.txt`, one list per server). On
start the newest is sent to the server as this computer's checkpoint (the server keeps the later one, never moves
back). That covers a checkpoint lost on the way, a changed office IP, and copies from before checkpoints (their
`production` folder is their baseline). Several computers behind one office IP share one checkpoint: one office, one
downloader.

The confirming `PUT` names the folder, so the server keeps a map of each office's downloads: the first ("everything"),
then each next one with the data it holds. `./Lutecia.command -history` (Windows: `Lutecia.exe -history`) prints this
computer's map and downloads nothing.

**Fresh start** (`./Lutecia.command -fresh`, Windows: `Lutecia.exe -fresh`; on purpose not a double-click): the
server forgets this office's checkpoint and map (`DELETE /downloads/checkpoint`, this IP only), then every dated
folder, an old `production` folder and the downloaded list move into `Lutecia for Bitrix/Before fresh start <date
time>/` (nothing reads it; delete it when done), and this run is a first download: `<date time> (everything)` with
`test/`, entry 1 of the map. Server first: if the server refuses, nothing moves; if a move fails (a file open in
Excel), what was moved goes back, so a plain double-click is safe; close the file and run `-fresh` again. Delete
the aside folder as a whole: it holds customer data, and its Excel copies are not cleaned up like the dated ones'. Deals already in Bitrix double if the new folder's deal files are imported.

**Starting over** (files deleted before import, or a fresh Bitrix portal): on the server run
`node dist/cli.js run --full --force-full --with-personal-data` (see lutecia-heartbeat's README). The server then makes
one bundle with everything (all products and contacts, the last year of deals); the client's next normal
double-click downloads it. Delete the year's deals in Bitrix first if they were imported, or they double.
