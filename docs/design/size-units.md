# Size display: decimal units, one formatter per runtime

**Decision (2026-11-09): every human-readable byte count in the product uses decimal units.**
`1 KB = 1000 bytes`, labels are `KB / MB / GB / TB`, and `KiB / MiB / GiB / TiB` never appear.
This is the convention macOS Finder and `ls -h` use, so the number a user reads here matches the
number their file manager shows for the same file.

Raw byte counts stay integers wherever a machine or a developer reads them (JSON, headers, the
database, logs, CLI diagnostics). Only text a person reads is formatted.

## Why

A 21,253,968,461-byte clip showed `19.8 GB` in the upload row while Finder showed `21.25 GB` for
the same file. Both numbers were arithmetically correct — the page divided by 1024 and labelled the
result `GB` — but the mismatch looks like compression or data loss. Decimal units remove the
1.0737× gap and the question that comes with it.

## The formatting rule

1. Under 1000 bytes print exact bytes: `137 B`.
2. Otherwise divide by 1000 until the value is below 1000, and use KB, MB, GB, TB in that order.
3. Precision per unit follows Finder's file style: **KB 0 decimals, MB 1, GB 2, TB 2**.
4. Trim trailing zeros: `3.90 GB` prints `3.9 GB`, `2.00 GB` prints `2 GB`.
5. If rounding reaches 1000 in the chosen unit, promote instead of printing four digits:
   `999,900 B` prints `1 MB`, not `1000 KB`.
6. Values of 1000 TB and above stay in TB (there is no PB unit; the service caps one file at 48 GB).
7. A missing size renders as `—` in lists (folders), and a non-positive count as `0 B` in progress
   text. Neither is a formatted size.

## Shared table

Both implementations must produce exactly this. A new row is added to all three places at once:
this table, `web/tests/sizeFormat.test.mjs`, `backend/internal/sizeformat/size_test.go`.

| bytes | text |
|---|---|
| 0 | `0 B` |
| 137 | `137 B` |
| 999 | `999 B` |
| 1000 | `1 KB` |
| 383000 | `383 KB` |
| 27800000 | `27.8 MB` |
| 999900 | `1 MB` |
| 2120000000 | `2.12 GB` |
| 21253968461 | `21.25 GB` |
| 3900000000 | `3.9 GB` |
| 14257400000 | `14.26 GB` |
| 1000000000000 | `1 TB` |
| 1500000000000 | `1.5 TB` |
| 999999999999 | `1 TB` |

## Implementations

| Runtime | Where | Entry points |
|---|---|---|
| Browser | `web/src/pages/sizeFormat.ts` | `formatBytes`, `formatSize` (`—` for zero), `formatSpeed` (`…/s`), `formatPreciseBytes` (always two decimals, for progress ticks that must visibly move) |
| Server | `backend/internal/sizeformat/size.go` | `Bytes(int64)` |

There is no third implementation. A new surface (a share page, an email, a CLI summary aimed at the
user) imports one of these; it does not divide by 1000 inline.

Two functions intentionally deviate and are not part of the table: `formatPreciseBytes` keeps two
decimals so a per-second progress tick always moves, and `formatSpeed` reuses `formatSize` and
appends `/s`.

## Declaring limits

A byte limit that appears in user text is declared in decimal, then rendered through the formatter,
so the constant and the sentence cannot disagree:

```go
maxFileSize = int64(48) * 1000 * 1000 * 1000 // "a single file is limited to 48 GB"
MaxHTMLLen  = 32 * 1000                      // "html card needs html under 32 KB"
```

Never hard-code a size into a message string. The quota refusal does the same:
`not enough space: need 21.25 GB, quota is 48 GB`.

Limits that used to be binary were redefined in decimal when this landed
(`48 GiB → 48 GB`, `32 KiB → 32 KB`), keeping the number a user already knew while making it true.

## What stays exact

- API resources and capability payloads (`size`, `quotaBytes`, `maxHTMLBytes`) carry integers.
- `Content-Length` and `Content-Range` are integers.
- Database rows, logs, and CLI diagnostics (`eam doctor` writes `%d bytes`) stay integer: they are
  read by developers and scripts, and rounding there hides the evidence.
- Prose in `README.md` and `docs/` may round for readability (`~20–40KB` thumbnails) but must not
  claim binary precision.

## History

- 2026-11-09: upload rows divided by 1024 under `GB` labels, so a 21.25 GB file read as `19.8 GB`.
  The frontend moved to decimal (main@5ee368e); the backend, its limits, and this rule followed.
