# Kotatsu-Redo MangaFire parser

`server/mangafire.go` adapts request canonicalization, VRF signing, constants,
and chapter/page API handling from `MangaFireParser.kt` in:

https://github.com/Kotatsu-Redo/kotatsu-parsers-redo

Pinned revision: `dc223c3d8098415b29cf1acfedc401a88d01c9fe`.
Upstream file: `src/main/kotlin/org/koitharu/kotatsu/parsers/site/all/MangaFireParser.kt`.
Copyright belongs to the Kotatsu parser contributors. Upstream is licensed
under GNU GPL v3; a copy is included in COPYING. The Go adaptation uses the
same license. Preserve these notices and the corresponding source when
redistributing this server.

The port omits browsing/search and volume bundles, preserves chapter numbers
as decimal strings instead of converting through Float, and handles all chapter
languages in one paginated feed. Networking uses Kotodex's public-address-only
client and its default Go user-agent; it does not import browser state.
