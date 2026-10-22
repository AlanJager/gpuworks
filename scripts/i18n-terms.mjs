/** Return glossary tokens that appeared in the source but were not preserved verbatim. */
export function missingProtectedTerms(source, target, terms) {
  return terms.filter((term) => source.includes(term) && !target.includes(term));
}
