# Alma Item Status Pulse

A small Go terminal dashboard for live Alma item status monitoring via Analytics.

The table resizes with the terminal window and refreshes automatically every 5 minutes.

The visible columns are Process State, Title, Barcode, and Call Number, and filters cycle horizontally with counts.

## Build a binary

Run this from the project folder:

go build -o alma-pulse .

## Run it locally

./alma-pulse

## Optional install

mkdir -p ~/.local/bin
cp alma-pulse ~/.local/bin/

If that folder is on your PATH, you can then run:

alma-pulse

## Environment

Set these values in your local env file:

- ALMA_API_BASE_URL
- ALMA_API_KEY
- ALMA_ANALYTICS_REPORT_PATH
- ALMA_ANALYTICS_FILTER (optional)
- ALMA_ANALYTICS_LIMIT (optional, 25-1000)

The analytics report should live in a Shared folder and include item-level rows with fields such as title, barcode, call number, status, process type, availability, and location.

By default, the app only keeps rows where process type is present and not equal to `NONE`.
If you want Alma to do that filtering before rows are paged back to the app, set `ALMA_ANALYTICS_FILTER` to a URL-encoded OBI `sawx:expr` filter taken from the report's Advanced XML.
The fetcher now converts each Analytics page into item records immediately instead of retaining the full raw report in memory.
Automatic refreshes are skipped while a prior Alma fetch is still running, so long report pulls do not stack overlapping requests.

## Controls

- q quit
- r refresh
- s sort cycle
- ! process state sort
- @ title sort
- # call number sort
- f next filter
- 0-9 direct filter shortcuts
- / search
- ctrl+l clear search
- e export visible rows to CSV
