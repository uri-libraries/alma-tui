# Alma Fulfillment Pulse

A small Go terminal dashboard for live Alma loan monitoring.

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
- ALMA_USER_GROUP

## Controls

- q quit
- r refresh
- s sort
- f filter
- / search
- ctrl+l clear search
- e export visible rows to CSV
