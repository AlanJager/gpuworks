# Translation workflow

English is the source language. UI locale files and all six server-owned
dictionaries are listed in `i18n.config.json`.

1. Copy `secrets/i18n-llm.env.example` to `secrets/i18n-llm.env` and configure
   the translator and reviewer OpenAI-compatible endpoints. The real file is
   ignored by git.
2. Run `make i18n-translate`. Only missing target keys are sent to the
   translator and written back; existing translations are never overwritten.
3. Run `make i18n-review`. Only new or hash-changed source/translation pairs
   are reviewed. The reviewer never edits locale files. Results are persisted
   in `review-state.json`; rejected entries are listed in
   `review-report.md` for human correction.
4. Correct rejected locale values manually and rerun `make i18n-review`.
5. Run `make i18n-check` for the secret-free mechanical checks used by CI.

`review_sample_rate` is currently `0.02` (2%). Sampling is deterministic
within each persisted review round; pending entries participate again in the
next round. Entries already reviewed with unchanged hashes are never repeated.
Previously rejected entries also remain unchanged until a human edits the
source or translation. Once edited, they are re-reviewed unconditionally and
do not depend on the 2% random sample. A remediation run containing forced
rechecks sends only those corrected entries; it does not start another random
sample and accidentally inflate the configured review rate.

If the corresponding LLM configuration is absent, translate/review prints a
skip message and exits successfully. CI only runs `make i18n-check`, ESLint,
the production build, and ordinary tests; it never reads secrets or calls an
LLM.
