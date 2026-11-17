# ZIP preview

Status: implemented in the web client. Code: `web/src/components/preview/zipArchive.ts` (reader),
`ZipPreview.tsx` (the two-pane UI), `web/tests/zipArchive.test.mjs` (fixtures and limits).

## The question

A file's detail page answers "what is this", and for an archive that means "what is inside". Before this,
a `.zip` fell through to *This format can't be previewed* with a download button. A 58 MB archive is a
real case: the answer is a few hundred names, and paying 58 MB for them makes opening the page as
expensive as downloading the file.

## Decision: the browser reads the archive

A ZIP keeps its directory in one contiguous block at the end and gives every entry an offset. So:

- **listing** = two ranged reads: the end-of-directory record (last ≤64 KiB + 22 bytes) and the central
  directory it points at. A 58 MB archive answers in a few kilobytes.
- **one entry** = one ranged read of its local header plus its compressed bytes, inflated with the
  browser's `DecompressionStream("deflate-raw")`.

The bytes still come from the read grant the detail page already obtains (`POST /api/files/downloads`);
the browser asks for ranges of the granted URL exactly as the PDF renderer already does. No route, no
documented API and no storage-driver capability were added.

### Why not a server endpoint

- **The service is not in the byte path today, and a `/archive` route would put it there** — for a parse
  the client can do with the same guarantee. Every preview would stream compressed bytes through Go,
  hold decompression state per request and spend shared CPU on data the user already paid to transfer.
- **The native tree stays authority.** Nothing is extracted to a filesystem, nothing is cached in
  Postgres, and there is no second representation of the file to keep consistent.
- **The grant already carries the security decisions** (owner, ACL, content revision, expiry). A ZIP
  reader needs no new authority: it reads whatever the grant already allows.

A server-side listing would only be justified by a client that cannot parse (a non-browser integration).
That is a different contract and would have to be designed in `docs/api/api.md`, not bolted onto the
content route.

## Limits, each with its reason

| Limit | Value | Why |
|---|---|---|
| Largest inflated entry | 128 MB | The entry is held in memory to hand to a renderer; above this a page cannot usefully preview it, and a decompression bomb would be bounded only by what the archive claims. |
| Central directory | 64 MB | Held in memory to be parsed. |
| Entries | 200 000 | A list a person browses; beyond it the filter is not a UI. |
| Compression methods | stored (0), deflate (8) | The only methods the browser can inflate natively; bzip2/zstd/lzma answer "unsupported" and the entry still downloads. |
| Encrypted archives | refused | No password prompt in this version; guessing is not a feature. |
| Rendered rows | 500 per level | The filter reaches the rest; the footer states how many matched. |

**Fallback instead of failure.** When the driver reports `RangeRead: false`, a provider answers `200` to
a range request, or the browser's CORS path rejects `Range`, the source downloads the object once and
serves every later read from memory. The worst case is the behavior the page had before, so the feature
never fails where a download works.

## Names are bytes with a flag that is often wrong

The name's encoding is declared by bit 11 of the general-purpose flags, and many Windows tools write a
GBK name without setting it. The reader tries UTF-8 (strict), then GBK/GB18030, then Latin-1, and only
then gives up. That is the difference between `工作.txt` and `?§?.txt` for the people most likely to
upload a Chinese zip.

## Nested archives are drilled into

Selecting a `.zip` inside a `.zip` pushes a level with a breadcrumb and a Back button, rather than
embedding a second browser inside the preview pane: embedding nests a sidebar inside a sidebar and the
way back becomes ambiguous.

## What would change this decision

- A client that cannot parse archives → an explicit archive resource in the API.
- Entries above 128 MB becoming routine → a streaming renderer instead of a Blob, which changes the
  preview contract rather than this reader.
- Password-protected archives as a product requirement → a prompt and a decrypt path, still client-side.
