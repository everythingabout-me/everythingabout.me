# Compressed file preview

Status: implemented in the web client. Code: `web/src/components/preview/compression.ts` (names, caps,
gzip, the layer chain), `web/src/components/preview/zstd.ts` (the Zstandard decoder and its frame bound),
`CompressedPreview.tsx` (the panel), `web/tests/compression.test.mjs` and `zstdInflate.test.mjs`.

## The question

A session log saved as `session.v4.jsonl.zstd` is a JSONL file. The detail page used to answer *This
format can't be previewed*, which is true of the bytes and false about the file: everything needed to
render it is one inflate away. The alternative — download, uncompress locally, open elsewhere — is a
round trip through a file manager for a file the browser can already read.

## Decision: the browser inflates, and the inner name picks the renderer

A single-stream compression has **no index**: unlike a ZIP, there is no central directory at the end to
range-read (see [zip-preview](zip-preview.md) for why that matters there), so a preview necessarily reads
the whole object once. Given the bytes, the inner name says what they are — `session.v4.jsonl` from
`session.v4.jsonl.zstd` — and that name goes through the same plugin chain as an upload:

- `.gz` → `DecompressionStream("gzip")`, the browser's own decoder, inflated incrementally so the output
  cap is enforced while it grows.
- `.zst` / `.zstd` → `fzstd`, a pure-JavaScript decoder (~24 KB) loaded by `import()`, so an image
  preview never pays for it. The browser has no `DecompressionStream("zstd")`; it understands Zstandard
  only as a transfer `Content-Encoding`, which is not an API.
- The result is handed to the viewer under the inner name: text and JSONL render as text, an image as an
  image, an office document as a document. If the inner name is itself an archive (`.zip.gz`), the
  archive browser opens it from an in-memory source instead of downloading it again.

The caption above the viewer names that inner file and its decompressed size, because the page above it
names the compressed one. For a text preview the viewer repeats both; for an image or the viewer's
own fallback card it is the only place the inner name appears.

No route, no documented API and no storage-driver capability was added: the bytes come through the read
grant the detail page already obtains, exactly as the ZIP reader and the PDF renderer do them. The
service never enters the byte path of a preview and nothing is extracted anywhere it would have to be
kept consistent.

### Why not a server endpoint

The same argument as [zip-preview](zip-preview.md#why-not-a-server-endpoint), and it holds more
strongly here because a server would have to *buffer the whole inflated file* to answer — it would add
CPU and memory per preview and a second representation of the same bytes, to save the client a parse it
can do with the same guarantee. A server-side endpoint is justified only by a client that cannot
decompress (a non-browser integration), and that is a different contract for `docs/api/api.md`.

## Limits, each with its reason

| Limit | Value | Why |
|---|---|---|
| Compressed object | 64 MB | Inflation is single-stream, so the input must be in memory before the first byte comes out; past this the page is a download, which is what it was before. |
| Decompressed content | 128 MB | Held in memory to hand to a renderer, and a decompression bomb has to be bounded by a number this code chose rather than by what the file claims. Same cap and reason as the archive reader's `MAX_ENTRY_BYTES`. |
| Bytes a frame may *claim* (Zstandard) | 128 MB declared content, 256 MB window | `fzstd` sizes its output from the frame header before decoding, so a claim past the limit is refused before the allocation. A frame with no declared size is allocated from its window — which describes back-references, not output (the zstd CLI declares 2 MiB, Node's streaming compressor 128 MiB) — hence the second, larger bound. |
| Compression layers | 3 | `a.gz` inside `a.gz` inside `a.gz` is already a curiosity; past this it is a bomb or a mistake. |
| Formats | `.gz`, `.zst`, `.zstd` | The browser decodes gzip; Zstandard is what this product's own tools write. Brotli, xz and bzip2 would each need a wasm module. |

**The name decides, not the media type**, for the reason the ZIP reader states: a stored media type is
often absent or generic, while the extension is what the viewer, the plugin chain and a person agree on.
A `.bin` that holds a Zstandard frame is therefore still not previewed — the honest fix for that file is
its name.

**`.tar.gz` previews as a `.tar`**, which has no renderer, so the viewer's own card appears and offers the
decompressed file. That is deliberate: tar is an archive format, not a compression, and a reader for it is
separate work, not a line in this one.

## Inside a ZIP

`supported.ts` now claims these extensions, which means the archive browser can be handed an entry like
`session.v4.jsonl.zstd` — its stored bytes are a Zstandard frame, and passing them to the text plugin
would render garbage. So an entry is inflated in memory, with the same caps and the same chain, before
the viewer sees it, and the inner name decides between a file preview and drilling into a nested archive.
The pane header keeps showing the *entry* name and size, because the pane's download button hands back
exactly those bytes.

## What would change this decision

- `DecompressionStream("zstd")` shipping in browsers → drop `fzstd` and the frame bound with it.
- A tar reader → `.tgz` and `.tar.gz` become archives rather than a card that offers the `.tar`.
- Previewing very large compressed files becoming routine → a streaming renderer (the viewer would take
  a stream instead of a `Blob`), which changes the preview contract rather than this reader.
- A client that cannot decompress → an explicit preview resource in the API.
