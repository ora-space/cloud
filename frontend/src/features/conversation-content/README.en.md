# Conversation content

[中文](README.md) | English

Safely renders user and agent Markdown; does not own requests, message storage or execution routing.

| File | Responsibility |
| --- | --- |
| `markdown.tsx` | `ConversationMarkdown`: GFM, code blocks, tables and safe links. |
| `clipboard.ts` | Browser boundary for explicit code-copy actions; the content component surfaces failures. |
| `markdown.test.tsx` | Real-renderer formatting and hostile-content regressions. |

Depends on React, react-markdown and remark-gfm; task and conversation views may depend on this module. Raw HTML is disabled; images render descriptions instead of loading third-party resources. Links use the Markdown renderer's default safe URL transform. Long code and tables scroll horizontally within their own regions. Tests use no network.
