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
the numbers, and opens the folder **`Lutecia for Bitrix`** (next to the program). It holds up to three folders with the same three sets of data:

- **`excel`**: copies for looking at the data. Double-click them; Georgian text, columns and barcodes show correctly.
  Never import these into Bitrix. The program overwrites them, so save your own work elsewhere. Delete them together
  with the `production` files: they hold customer data (the next double-click also removes them).
- **`production`**: the real data, the files to import. If you open one in Excel, don't save it.
- **`test`**: a small part of it (about 10 deals, their buyers and their products) for trying an import. The test
  deals have names starting with `TEST`: delete them in Bitrix before importing `production`, or those deals are there
  twice. Delete the `test` folder when you finish testing, because it holds customer data. It is remade only when
  `production` has enough data, and the closing message says when it is ready. Temporary: may be dropped later.

Import the files from `production` into Bitrix24 **in this order**, then **delete them**:

1. `products.csv` (CRM → Products → Import)
2. `offline_contact.csv` (CRM → Contacts → Import)
3. `offline_deals.csv` (CRM → Deals → Import)

Each double-click brings only what is new or changed since the last one, so Bitrix never gets anything twice.
Forgot to import before double-clicking again? Nothing is lost: the new data is added to the same files.
**Never delete the files before they are imported**: their data is not sent again (ask Sweeft if it happened).

`offline_deals.csv` holds the purchases of the last year (one deal per receipt, its products as rows). Bitrix import can
only add deals, never remove them, so **once a month** delete the old ones: CRM → Deals → list view → filter
"Sale date" before today minus one year → select all → Delete.

Columns with Bitrix's own names map by themselves. Create the others once in Bitrix as custom fields (product
properties for products) and map them at import:

- **Contacts:** Contact key, Loyalty card, Card discount %, Home store, Customer since, Purchases, Total spend,
  First purchase, Last purchase, Average purchase, Purchases (last year), Spend (last year), Returns,
  Favourite brand, Brands bought, Favourite category, Categories bought.
- **Deals:** Contact key, Sale date (a date field), Sale time, Loyalty card, Store, Seller, Payment, Items,
  Brands, Categories.
- **Products:** Brand, Color / line, Gender, Size, Volume, Barcode, Sold by.

**Contact key** is the customer's id: a deal's buyer is always the contact with the same Contact key. The import
links a deal to its contact by first and last name ("Contact"); when two contacts share a name that column is left
empty, so the deal is not attached to the wrong person, and Contact key still says who bought.
**Sale date** is the real date of the purchase: Bitrix may put the import day into Close date.

What the CRM can answer with this, for example: contacts with Favourite brand = TERENZI; contacts whose Brands bought
contains LANCOME and Last purchase is over 6 months ago; deals whose Brands contains DIOR in the last 3 months, by
Store or Seller; the best customers by Spend (last year).

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

How it works: the server (`lutecia-heartbeat`) makes a bundle per pull with only the new or changed rows. The program
fetches every bundle it has not had before (`POST /heartbeat/runs`, `GET /bundles`, `GET /bundles/:id/files/:name`, with
`Authorization: Bearer <token>`) and merges each into one file per entity in `production/` (the server's `contacts.csv` and `deals.csv` are saved as
`offline_contact.csv` and `offline_deals.csv`; `src/testslice.go` then rebuilds `test/` from them): a newer record replaces the one with the
same key (products: External ID; contacts: Contact key; deals: Deal key, the deal's rows moved as a whole and its
`ID` renumbered, since every bundle numbers its deals from 1). The list of downloaded bundles is kept in the user's
settings folder (`lutecia-downloader/` in `%AppData%`, `~/Library/Application Support` or `~/.config`), one per
server, so deleting the files never brings old data back.

**Starting over** (files deleted before import, or a fresh Bitrix portal): on the server run
`node dist/cli.js run --full --force-full --with-personal-data` (see lutecia-heartbeat's README). The server then makes
one bundle with everything (all products and contacts, the last year of deals); the client's next normal
double-click downloads it. Delete the year's deals in Bitrix first if they were imported, or they double.
