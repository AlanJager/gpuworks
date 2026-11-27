# ADR 0086: ui-check Walks No Pages for a Change That Only Adds Error Code Texts, a Deliberate Narrowing of Coverage

Date: 2026-11-27 · Status: Accepted

## Background

ui-check's "Walk the UI" picks the pages to walk from the changed files, using the import graph of `ui/src`. Locale JSON is not in the graph, so any locale change walks every page: each page at the 1440 and 1024 widths in Chinese and English, compared with a build of the base. A change that only adds one error text (#181, #194 and #196 each only added `apiError.IMAGE_MANIFEST_LEGACY_PROJECTION`) therefore took 10 to 26 minutes, while runs that walk 0 or 1 page take 2 to 5 minutes.

Nearly every backend change adds error code texts, so the walk time lands on most PRs.

The first proposal argued that such a change cannot render on any existing page, so skipping the walk loses no coverage. That argument does not hold: the console renders codes from the server generically (`` t(`apiError.${code}`) `` in `ui/src/apiError.ts`, and `productMessageText`, used for non-pass preflight items in deployment, diagnostic items, the update check error on the settings page, and elsewhere). When a PR adds a code and makes an existing endpoint return it, the new text shows on existing pages. The experience server ui-check runs is built from the PR's own code, and Go changes add no page to the walk, so those pages are not walked. Proving statically that a new code cannot reach existing pages would need an analysis of which Go endpoints can return it, which the script cannot do.

## Decision

1. **A locale change that only adds error code texts walks no pages.** `scripts/ui_locale_changes.mjs` decides the condition: it only adds keys; every language adds them; the keys are error code texts under `apiError.` in the `common` namespace; none is a plural or context variant of an existing key, or the base of existing variants; no untouched source file names it literally. Every other locale change (a changed value, a removed key or file, a key added in some languages only, other prefixes and namespaces) walks every page as before.

2. **This narrows coverage; it is not an equivalent rewrite.** The coverage given up: layout problems (too long, wrapping, overflow) of the new text on existing pages when the same PR makes an existing endpoint return the new code. The coverage kept: text length is held by the `ui_copy_message_strings` (80 Chinese characters) and `ui_copy_en_strings` budgets; a change to the components that render error codes walks the pages they are on as usual; when the PR also changes the page that renders the code, that page is walked as usual; the `detail-spacing` and vendor mark checks run as usual.

3. **On record.** When it skips, the CI log says `not walked (ADR 0086)` and lists the skipped keys; the page attribution section of `scripts/arch-constraints.md` says this is a narrowing and links this ADR.

4. **When it is taken back.** The skip is removed, and locale changes walk every page again, when any of these holds:
   - walking every page takes under 5 minutes (for example by walking pages in parallel or in shards);
   - ui-check can derive from the experience scenarios which error codes each page actually renders; the skip then applies only to codes not among them;
   - a layout problem of an error code text is found after merge that the skip let through.

## Trade-offs

- **Skipping vs walking every page as before**: walking every page loses no coverage, but every PR with a new error code waits 10 to 20 minutes more. The coverage given up concerns only the layout of error code texts, whose length is already held by budgets, so we skip and state when the skip is taken back.
- **Skipping only changes proven statically not to render vs this decision**: the former loses no coverage, but a new error code text always comes with a new code, and whether that code reaches existing pages depends on Go code the script cannot prove anything about, so almost no change could skip; it would amount to no change.

## Consequences

- PRs that only add error code texts no longer walk pages for their locale changes; layout problems of a new code on existing pages are left to the text budgets, review, and the scheduled ui-check after merge (scheduled runs walk every page).
- A deleted locale file walks every page as usual: the locales load by directory glob, so no importer changes with it.

<!-- translation-source-sha256: 906caa9b1d348a016f653619ecdedc19866cef18d97f47e721210d517c05a62b # ADR 0086: ui-check Walks No Pages for a Change That Only Adds Error Code Texts, a Deliberate Narrowing of Coverage -->
<!-- translation-source-sha256: 33ca8de99f1fb7665457313df553fba0fa019c4fe135e2cf24d0d4c957d6f125 ## Background -->
<!-- translation-source-sha256: afe8470c7956fb3d84608eba4e69ea61dd24a4cf395471cf8d188f5470ce3158 ## Decision -->
<!-- translation-source-sha256: eab6b35fb5f8b2e2973d56bf2a0e3fb3e8501a8a0c577824a796c95b88934009 ## Trade-offs -->
<!-- translation-source-sha256: 0372363e732c40563710a9a5a47f2257287da28d11d222d2bcbb2cb8c6b293c0 ## Consequences -->
