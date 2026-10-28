# Email templates

Reference copies of the SES template used for sign-in / sign-up / email-change verification codes.
The wording has to work for all three flows, so it never says "sign up" or "confirm your new address".

| File | Locale |
|---|---|
| `sign-in-code.en.txt` / `sign-in-code.en.html` | English (default) |
| `sign-in-code.zh.txt` / `sign-in-code.zh.html` | Chinese |

SES keeps one template per locale, so upload both and wire them up separately:

- `SES_TEMPLATE_ID` — the default (English) template id
- `SES_TEMPLATE_ID_ZH` — the Chinese template id; optional, required for Chinese-language browsers
- `SES_SUBJECT` / `SES_SUBJECT_ZH` — the matching subjects

The backend picks the locale from the browser's `Accept-Language` header for each request and falls back to
English. Keep the template variable (`{{code}}` by default) identical in both templates, and keep it in sync
with `SES_TEMPLATE_VAR`.
