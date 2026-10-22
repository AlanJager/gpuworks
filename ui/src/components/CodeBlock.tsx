import type { ComponentPropsWithoutRef, ReactNode } from "react";
import type { Element, ElementContent } from "hast";
import { useTranslation } from "react-i18next";
import CopyButton from "./CopyButton";

/** A fenced code block as react-markdown hands it to the `pre` renderer. */
export interface CodeBlockInfo {
  /** Fence info string language ("html" for ```html), lower-cased; "" if none. */
  language: string;
  /** The block's source text, without the newline the fence parser appends. */
  code: string;
  /**
   * Offset of the opening fence in the Markdown source. It stays put while a
   * streamed reply grows, so it identifies the block across re-renders.
   */
  startOffset?: number;
  /**
   * Whether the closing fence has arrived. A streamed block is open until
   * then; everything after an unclosed fence is still part of it.
   */
  closed: boolean;
}

type PreProps = ComponentPropsWithoutRef<"pre"> & { node?: Element };

/**
 * fenceClosed reports whether a block's source ends with a closing fence at
 * least as long as its opening one. An indented block has no fence and is
 * always complete.
 */
function fenceClosed(raw: string): boolean {
  const lines = raw.split("\n");
  const opening = lines[0].match(/^ {0,3}(`{3,}|~{3,})/);
  if (!opening) return true;
  if (lines.length < 2) return false;
  const marker = opening[1];
  const last = lines[lines.length - 1].trim();
  return last.length >= marker.length && [...last].every((char) => char === marker[0]);
}

/**
 * codeBlockInfo reads a fenced block out of the hast `pre` node. A `pre`
 * without a `code` child (hand-written HTML in a README) is not a fenced
 * block and yields undefined.
 */
export function codeBlockInfo(node: Element | undefined, source?: string): CodeBlockInfo | undefined {
  const code = node?.children.find((child): child is Element => child.type === "element" && child.tagName === "code");
  if (!code) return undefined;
  const classes = code.properties?.className;
  const languageClass = (Array.isArray(classes) ? classes : []).map(String).find((name) => name.startsWith("language-"));
  const start = node?.position?.start.offset;
  const end = node?.position?.end.offset;
  return {
    language: languageClass ? languageClass.slice("language-".length).toLowerCase() : "",
    code: textOf(code.children).replace(/\n$/, ""),
    startOffset: start,
    closed: source === undefined || start === undefined || end === undefined ? true : fenceClosed(source.slice(start, end)),
  };
}

function textOf(children: ElementContent[]): string {
  return children.map((child) => child.type === "text" ? child.value : child.type === "element" ? textOf(child.children) : "").join("");
}

/**
 * CodeBlock renders a fenced block with a header carrying its language, any
 * caller-supplied actions and a copy button. Other `pre` elements render
 * unchanged.
 */
export default function CodeBlock({ node, source, actions, ...props }: PreProps & { source?: string; actions?: (block: CodeBlockInfo) => ReactNode }) {
  const { t } = useTranslation("common");
  const block = codeBlockInfo(node, source);
  if (!block) return <pre {...props} />;
  return (
    <div className="markdown-code-block">
      <div className="markdown-code-header">
        <span className="markdown-code-language">{block.language || "text"}</span>
        <span className="markdown-code-actions">
          {actions?.(block)}
          <CopyButton size="small" label={t("copy.code")} text={block.code} />
        </span>
      </div>
      <pre {...props} />
    </div>
  );
}
