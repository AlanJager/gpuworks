import assert from "node:assert/strict";
import { test } from "node:test";

const { editErrorOf, yamlModelID } = await import("../src/pages/inferenceTemplateFlat.ts");

test("yamlModelID takes the first top-level model identity in quoted and bare forms", () => {
  assert.equal(yamlModelID('models:\n- model_id: "org/model"\n  specs: []'), "org/model");
  assert.equal(yamlModelID("models:\n- model_id: 'org/model'\n"), "org/model");
  assert.equal(yamlModelID("models:\n- model_id: org/model\n"), "org/model");
  assert.equal(yamlModelID('models:\n- model_id: "first/one"\n- model_id: "second/one"\n'), "first/one");
  // the server writes the models list indented under its key, at any depth
  assert.equal(yamlModelID('models:\n  - model_id: org/indented\n    source: user\n'), "org/indented");
  assert.equal(yamlModelID('models:\n    - model_id: "org/deep"\n'), "org/deep");
  assert.equal(yamlModelID("models:\n\n  # the template\n  - model_id: org/commented\n"), "org/commented");
  // nested items and indented items outside the models list do not qualify
  assert.equal(yamlModelID('models:\n- name: x\n  specs:\n    - model_id: "org/nested"\n'), undefined);
  assert.equal(yamlModelID('other:\n  - model_id: "org/elsewhere"\n'), undefined);
  assert.equal(yamlModelID("models: []"), undefined);
  assert.equal(yamlModelID(""), undefined);
  // a retyped document in front of stale content wins, which is what the
  // save-time overwrite confirmation keys on
  assert.equal(yamlModelID('- model_id: "new/one"\n- model_id: "old/one"\n'), "new/one");
});

test("editErrorOf keeps the reason and line number but drops Go decoding noise", () => {
  const syntax = editErrorOf("parse inference catalog YAML: yaml: line 7: did not find expected node content");
  assert.equal(syntax.line, 7);
  assert.equal(syntax.message, "line 7: did not find expected node content");

  const unknown = editErrorOf("parse inference catalog YAML: yaml: unmarshal errors:\n  line 51: field enginex not found in type inferencecatalog.InferenceSpec");
  assert.equal(unknown.line, 51);
  assert.equal(unknown.message, "line 51: field enginex not found");
  assert.ok(!/inferencecatalog/.test(unknown.message), "Go type names stay out of the user-facing panel");

  const duplicate = editErrorOf('yaml: unmarshal errors:\n  line 45: mapping key "cc" already defined at line 45');
  assert.equal(duplicate.line, 45);
  assert.match(duplicate.message, /mapping key "cc" already defined/);

  const noLine = editErrorOf("inference catalog is empty");
  assert.equal(noLine.line, undefined);
  assert.equal(noLine.message, "inference catalog is empty");

  // pathological input never yields an empty panel
  const stripped = editErrorOf("yaml: line 3:");
  assert.equal(stripped.message, "line 3:");
});
