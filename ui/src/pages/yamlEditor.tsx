import { useEffect, useRef } from "react";
import CodeMirror from "@uiw/react-codemirror";
import { EditorSelection } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { yamlSchema } from "codemirror-json-schema/yaml";

export interface YamlEditorFocus { line: number; nonce: number }

interface YamlEditorProps {
  value: string;
  onChange: (value: string) => void;
  schema?: unknown;
  ariaLabel: string;
  errorFocus?: YamlEditorFocus;
}

// CodeMirror-backed half of the template YAML editor. Loaded lazily so the
// editor stack stays out of the main bundle; environments without
// requestAnimationFrame (test runners, restricted embedders) keep the plain
// textarea implementation in InferenceTemplateDetailPage.
export default function YamlEditor({ value, onChange, schema, ariaLabel, errorFocus }: YamlEditorProps) {
  const viewRef = useRef<EditorView | null>(null);
  const extensions = [
    EditorView.contentAttributes.of({ "aria-label": ariaLabel }),
    ...(schema ? [yamlSchema(schema as never)] : []),
  ];
  useEffect(() => {
    if (!errorFocus || !viewRef.current) return;
    const view = viewRef.current;
    const lineNumber = Math.max(1, Math.min(errorFocus.line, view.state.doc.lines));
    const line = view.state.doc.line(lineNumber);
    view.dispatch({
      selection: EditorSelection.range(line.from, line.to),
      effects: EditorView.scrollIntoView(line.from, { y: "center" }),
    });
    view.focus();
  }, [errorFocus]);
  return <div className="template-yaml-codemirror">
    <CodeMirror
      value={value}
      onChange={onChange}
      onCreateEditor={(view) => { viewRef.current = view; }}
      extensions={extensions}
      basicSetup={{ foldGutter: true, highlightActiveLine: true, searchKeymap: true }}
    />
  </div>;
}
