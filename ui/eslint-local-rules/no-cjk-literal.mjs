const CJK = /\p{Script=Han}/u;

function ignored(node, sourceCode) {
  return sourceCode.getAllComments().some((comment) =>
    comment.value.includes("i18n-ignore") &&
    (comment.loc.end.line === node.loc.start.line || comment.loc.end.line === node.loc.start.line - 1));
}

function check(node, value, context) {
  if (typeof value !== "string" || !CJK.test(value)) return;
  if (ignored(node, context.sourceCode)) return;
  context.report({ node, messageId: "cjkLiteral" });
}

export default {
  meta: {
    type: "problem",
    docs: { description: "Disallow untranslated CJK text in migrated UI source files" },
    schema: [],
    messages: {
      cjkLiteral: "Move user-facing CJK text into a locale resource, or document a line-level i18n-ignore exemption.",
    },
  },
  create(context) {
    return {
      Literal(node) {
        check(node, node.value, context);
      },
      TemplateElement(node) {
        check(node, node.value.raw, context);
      },
      JSXText(node) {
        check(node, node.value, context);
      },
    };
  },
};
