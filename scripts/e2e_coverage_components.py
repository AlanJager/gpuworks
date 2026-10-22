#!/usr/bin/env python3
"""Derived component declarations; percentages never imply execution or page coverage."""
from __future__ import annotations

import ast
import itertools
import re
from pathlib import Path

from e2e_coverage_inventory import _safe_file
from e2e_coverage_values import value_rows

STANDARD = "test/e2e/browser-e2e-component-checklists.md"
_BINDINGS = {"checklist_items", "partial_checklist", "checklist", "component_checklist"}


def _text(root: Path, relative: str) -> str:
    path = _safe_file(root, relative)
    try:
        return path.read_text() if path else ""
    except (OSError, UnicodeError):
        return ""


def _sections(root: Path) -> dict:
    sections, current = {}, None
    for line in _text(root, STANDARD).splitlines():
        heading = re.match(r"## (\d+)\. (.+)", line)
        if heading:
            current = heading[1]
            sections[current] = {"name": heading[2], "items": {}}
        elif line.startswith("## "):
            current = None
        elif current:
            item = re.match(r"(\d+)\.\s+(.+)", line)
            if item:
                sections[current]["items"][current + "." + item[1]] = item[2].replace("**", "")
    return sections


def _ids(value: str, sections: dict) -> list[str]:
    ids = set()
    for section, first, last_section, last in re.findall(r"(\d+)\.(\d+)\s*[-–]\s*(\d+)\.(\d+)", value):
        if section == last_section and int(last) - int(first) < 100:
            ids.update(f"{section}.{number}" for number in range(int(first), int(last) + 1))
    ids.update(re.findall(r"\b\d+\.\d+\b", value))
    return sorted((item for item in ids if item.split(".")[0] in sections
                   and item in sections[item.split(".")[0]]["items"]), key=lambda item: tuple(map(int, item.split("."))))


def _status(value: str, inherited: str, explicit: bool = False) -> str:
    upper = value.strip().upper()
    if re.match(r"^(?:UNKNOWN|PENDING|NOT_RUN|UNREGISTERED)(?:$|:|\s)", upper):
        return "unknown"
    if upper.startswith(("N/A", "NOT_APPLICABLE")):
        return "not_applicable"
    if upper.startswith(("MISSING", "NOT_COVERED", "FAIL_TEST")):
        return "missing"
    if "PARTIAL" in upper or "MISSING" in upper or "NOT_COVERED" in upper:
        return "partial"
    if explicit and upper != "COVERED":
        return "unknown"
    return inherited


def _bindings(details: dict, sections: dict) -> dict:
    """Read explicit checklist mappings only; generic dimension labels are not proofs."""
    result = {}

    def register(label, value, status, path):
        item_ids = _ids(str(label), sections)
        section_only = str(label).strip() in sections
        if section_only:
            # A whole-section summary cannot assert that every item is covered.
            item_ids = list(sections[str(label).strip()]["items"])
            if status == "covered":
                status = "partial"
        if status == "not_applicable" and not value.strip():
            status = "unknown"
        for item_id in item_ids:
            result.setdefault(item_id, []).append((status, value, path))

    def walk(value, path, inherited="covered"):
        if isinstance(value, list):
            for index, entry in enumerate(value):
                if isinstance(entry, str):
                    register(entry, "" if inherited == "not_applicable" else "Declared checklist binding " + entry, inherited, f"{path}/{index}")
                else:
                    walk(entry, f"{path}/{index}", inherited)
        elif isinstance(value, dict):
            if "item" in value:
                state = _status(str(value.get("status", "")), inherited, explicit="status" in value)
                reason = str(value.get("reason", value.get("assertion", ""))).strip()
                if state == "not_applicable" and reason.upper() in {"", "N/A", "NOT_APPLICABLE"}:
                    state = "unknown"
                register(value["item"], reason, state, path)
                return
            for key, entry in value.items():
                states = {"missing": "missing", "partial": "partial", "not_applicable": "not_applicable",
                          "covered": "covered", "covered_in_this_scenario": "covered"}
                if key in states:
                    walk(entry, f"{path}/{key}", states[key])
                elif _ids(key, sections) or key in sections:
                    if isinstance(entry, str):
                        state = _status(entry, inherited)
                        reason = entry
                        if state == "not_applicable":
                            reason = re.sub(r"^(?:N/A|NOT_APPLICABLE)\s*[:;—-]?\s*", "", entry, flags=re.I)
                        register(key, reason, state, f"{path}/{key}")
                    elif isinstance(entry, dict):
                        walk({"item": key, **entry}, f"{path}/{key}", inherited)
                elif isinstance(entry, (dict, list)):
                    walk(entry, f"{path}/{key}", inherited)

    for key in _BINDINGS:
        if key in details:
            walk(details[key], "/" + key, "partial" if key == "partial_checklist" else "covered")
    # Several manifests place explicit checklist bindings under dimensions.
    dimensions = details.get("dimensions")
    if isinstance(dimensions, dict):
        for key in _BINDINGS:
            if key in dimensions:
                walk(dimensions[key], "/dimensions/" + key, "partial" if key == "partial_checklist" else "covered")
    inherited = details.get("inherited_declarations")
    if isinstance(inherited, dict):
        for item_id, bindings in _bindings(inherited, sections).items():
            result.setdefault(item_id, []).extend(bindings)
    return result


def _row(group: list[dict], suffix: str, name: str, dimension: str) -> dict:
    first = group[0]
    return {"id": first["manifest"] + "#" + first["module"] + "/" + suffix, "name": name, "module": first["module"],
            "contract_ids": [item["id"] for item in group], "manifest": first["manifest"],
            "dimension": dimension, "covered": [], "missing": [], "unknown": [],
            "not_applicable": [], "numerator": None, "denominator": None, "percent": None,
            "basis": "", "assertions": [], "source": first["manifest"]}


def _checklists(group: list[dict], sections: dict) -> list[dict]:
    bindings = {}
    for contract in group:
        for item_id, values in _bindings(contract.get("details", {}), sections).items():
            bindings.setdefault(item_id, []).extend(values)
    rows = []
    for section_id in sorted({item.split(".")[0] for item in bindings}, key=int):
        section = sections[section_id]
        row = _row(group, "checklist-" + section_id, section_id + ". " + section["name"], "checklist")
        for item_id, name in section["items"].items():
            declarations = bindings.get(item_id, [])
            states = {state for state, _, _ in declarations}
            label = item_id + " " + name
            if not states:
                row["unknown"].append(label)
            elif states == {"covered"}:
                row["covered"].append(label)
            elif states == {"not_applicable"}:
                row["not_applicable"].append(label + ": " + declarations[0][1])
            elif "missing" in states or "partial" in states:
                row["missing"].append(label + " [partial]" if "partial" in states else label)
            else:
                row["unknown"].append(label + " [conflicting or incomplete declaration]")
            row["assertions"].extend(sorted({f"{path}: {reason}" for _, reason, path in declarations}))
        row["numerator"] = len(row["covered"])
        row["denominator"] = len(section["items"]) - len(row["not_applicable"])
        row["percent"] = round(row["numerator"] * 100 / row["denominator"], 1) if row["denominator"] else None
        row["basis"] = "规范项声明覆盖：分母为该节全部规范项减有理由 N/A；partial、缺口与未登记项均不计分子。不是运行通过率或页面完成度。"
        row["source"] = STANDARD + "#" + section_id
        rows.append(row)
    return rows


def _parse(source):
    try:
        return ast.parse(source)
    except SyntaxError:
        return ast.Module(body=[], type_ignores=[])


def _shape(node, text):
    return ast.dump(node, include_attributes=False) == ast.dump(ast.parse(text).body[0], include_attributes=False)


def _unchanged(scope, names, allowed):
    allowed_stores = {id(node) for parent in allowed for node in ast.walk(parent)
                      if isinstance(node, ast.Name) and isinstance(node.ctx, ast.Store)}
    for node in ast.walk(scope):
        if isinstance(node, ast.Name) and node.id in names and isinstance(node.ctx, ast.Store) and id(node) not in allowed_stores:
            return False
        if isinstance(node, (ast.Subscript, ast.Attribute)):
            base = node.value
            while isinstance(base, (ast.Subscript, ast.Attribute)):
                base = base.value
            if isinstance(base, ast.Name) and base.id in names:
                if isinstance(node.ctx, ast.Store) or isinstance(node, ast.Attribute):
                    return False
    return True


def _literal_assignments(source: str) -> dict:
    tree, values = _parse(source), {}
    for node in tree.body:
        if not (isinstance(node, ast.Assign) and len(node.targets) == 1 and isinstance(node.targets[0], ast.Name)):
            continue
        name = node.targets[0].id
        if not _unchanged(tree, {name}, [node.targets[0]]):
            continue
        try:
            values[name] = ast.literal_eval(node.value)
        except (ValueError, TypeError):
            comp = node.value
            if not (isinstance(comp, ast.ListComp) and len(comp.generators) == 1):
                continue
            generator = comp.generators[0]
            if generator.ifs or generator.is_async or not isinstance(generator.target, ast.Name):
                continue
            identity = lambda item: isinstance(item, ast.Name) and item.id == generator.target.id
            pair = isinstance(comp.elt, ast.Tuple) and len(comp.elt.elts) == 2 and all(identity(item) for item in comp.elt.elts)
            if not identity(comp.elt) and not pair:
                continue
            try:
                items = ast.literal_eval(generator.iter)
                values[name] = [tuple(item for _ in comp.elt.elts) for item in items] if pair else list(items)
            except (ValueError, TypeError):
                pass
    return values


def _function(tree, name):
    return next((node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name == name), None)


def _assignment(scope, target):
    return next((node for node in scope.body if isinstance(node, ast.Assign) and len(node.targets) == 1
                 and isinstance(node.targets[0], ast.Name) and node.targets[0].id == target), None)


def _source_flow(source, mode):
    """Recognize bounded existing dataflow; unsupported rebinding/filtering is unknown."""
    tree = _parse(source)
    names = {"image": "test_image_enum_options", "token": "test_platform_token_option",
             "network": "test_type_role_combination", "filter": "test_image_series_filters"}
    function = _function(tree, names[mode])
    if not function:
        return False
    statements = {"image": "field, options = dimensions[dimension]", "token": "scope, days = CASES[case]",
                  "network": "kind, roles = COMBINATIONS[key]"}
    if mode in statements:
        binding = next((node for node in function.body if _shape(node, statements[mode])), None)
        if not binding:
            return False
        targets = binding.targets
        tracked = {node.id for target in targets for node in ast.walk(target) if isinstance(node, ast.Name)}
        tracked.update({"case"} if mode == "token" else {"key"} if mode == "network" else set())
        if not _unchanged(function, tracked, targets):
            return False
    if mode == "image":
        selector = _assignment(function, "dimension")
        selector_value = selector.value if selector else None
        if not isinstance(selector_value, ast.Subscript) or not isinstance(selector_value.value, ast.Dict) or not _shape(ast.Expr(selector_value.slice), 'report["contract"]'):
            return False
        mapping = {key.value.rsplit("/", 1)[-1]: value.value for key, value in zip(selector_value.value.keys, selector_value.value.values)
                   if isinstance(key, ast.Constant) and isinstance(value, ast.Constant) and isinstance(key.value, str)}
        if mapping != {"engine-options": "engines", "framework-options": "frameworks", "architecture-options": "architectures"} or not _unchanged(function, {"dimension"}, selector.targets):
            return False
        dimension = _assignment(function, "dimensions")
        if not dimension or not isinstance(dimension.value, ast.Dict) or not _unchanged(function, {"dimensions"}, dimension.targets):
            return False
        expected = {"frameworks": ("framework", "FRAMEWORKS"), "architectures": ("hostArch", "ARCHITECTURES")}
        entries = {key.value: value for key, value in zip(dimension.value.keys, dimension.value.values) if isinstance(key, ast.Constant)}
        for key, (field, constant) in expected.items():
            value = entries.get(key)
            if not isinstance(value, ast.Tuple) or len(value.elts) != 2 or not isinstance(value.elts[0], ast.Constant) or value.elts[0].value != field or not isinstance(value.elts[1], ast.Name) or value.elts[1].id != constant:
                return False
        loop = next((node for node in function.body if isinstance(node, ast.For) and
                     _shape(ast.Expr(node.iter), "enumerate(options)")), None)
        if not loop or not _unchanged(function, {"value", "label", "index"}, [loop.target]):
            return False
        if any(isinstance(node, (ast.Break, ast.Continue)) for node in ast.walk(loop)):
            return False
        selected_values = []
        for name, expression in {"engine": 'engine = value if dimension == "engines" else engines[0]["name"]',
                                 "framework": 'framework = value if dimension == "frameworks" else "cuda"',
                                 "arch": 'arch = value if dimension == "architectures" else "multi"'}.items():
            assignment = next((node for node in loop.body if _shape(node, expression)), None)
            if not assignment:
                return False
            selected_values.extend(assignment.targets)
        if not _unchanged(function, {"engine", "framework", "arch"}, selected_values):
            return False
        if not any(_shape(node, 'check(response.request.post_data_json == payload, "selected enum wire value differs")') for node in loop.body):
            return False
    if mode == "token":
        return "CASES" in _literal_assignments(source)
    if mode == "network":
        expression = "COMBINATIONS = {kind + '--' + '-'.join(roles): (kind, list(roles)) for kind in TYPES for size in range(1, len(TYPE_ROLES[kind]) + 1) for roles in itertools.combinations(TYPE_ROLES[kind], size)}"
        combinations = _assignment(tree, "COMBINATIONS")
        if not combinations or not _shape(combinations, expression) or not _unchanged(tree, {"COMBINATIONS"}, combinations.targets):
            return False
        role_map = _assignment(tree, "TYPE_ROLES")
        if not role_map or not _unchanged(tree, {"TYPE_ROLES"}, role_map.targets):
            return False
        if not {"ROLES", "TYPES"}.issubset(_literal_assignments(source)):
            return False
    if mode == "filter":
        status = _assignment(function, "status")
        if not status or not _shape(status, 'status = [text["common"]["enabled"], text["common"]["disabled"]]'):
            return False
        outer = next((node for node in function.body if isinstance(node, ast.For) and
                      _shape(ast.Expr(node.iter), '[("engine", engines), ("framework", frameworks), ("enabled", status)]')), None)
        inner = next((node for node in outer.body if isinstance(node, ast.For) and _shape(ast.Expr(node.iter), "options")), None) if outer else None
        if not inner or not _unchanged(function, {"status", "field", "options", "option"}, [*status.targets, outer.target, inner.target]):
            return False
        if any(isinstance(node, (ast.Break, ast.Continue)) for node in ast.walk(inner)):
            return False
    return True


def _select_options(source: str, name: str) -> list:
    form = re.search(r'<Form\.Item\b[^>]*\bname="' + re.escape(name) + r'"[^>]*>(.*?)</Form.Item>', source, re.S)
    if not form:
        return []
    options = re.search(r"options=\{\[(.*?)\]\}", form[1], re.S)
    if not options:
        return []
    return re.findall(r'value:\s*"([^"]+)"', options[1])


def _metric(group, suffix, name, universe, selected, basis, assertions, dimension="enum", contract_map=None):
    row = _row(group, suffix, name, dimension)
    row["basis"], row["assertions"] = basis, assertions
    if universe is None:
        row["covered"] = sorted(map(str, selected))
        row["unknown"] = ["完整可选值集合无法从当前来源独立确定；不计算百分比"]
    else:
        full, claimed = list(map(str, universe)), set(map(str, selected))
        row["covered"] = [value for value in full if value in claimed]
        row["missing"] = [value for value in full if value not in claimed]
        row["numerator"], row["denominator"] = len(row["covered"]), len(full)
        row["percent"] = round(100 * len(row["covered"]) / len(full), 1) if full else None
        if claimed - set(full):
            row["unknown"].append("登记了不属于当前产品选项的值：" + ", ".join(sorted(claimed - set(full))))
    if contract_map is not None:
        row["value_contract_ids"] = contract_map
    return row


def _image_enums(root, group):
    details = group[0]["details"]
    dimensions = details.get("dimensions", {})
    if not isinstance(dimensions, dict) or "enum_options" not in dimensions:
        return []
    file = group[0].get("test_file", "")
    if file != "test/e2e/image_catalog_enums.py":
        return []
    source = _text(root, file)
    ui_file = "ui/src/pages/images/ImageImportDrawer.tsx"
    ui = _text(root, ui_file)
    fields = details.get("fields", [])
    field = next((field for field in fields if isinstance(field, dict) and field.get("component") == "Select"), {})
    field_id = field.get("field_id", "image.enum")
    names = {"image.architecture": ("arch", "ARCHITECTURES"), "image.framework": ("framework", "FRAMEWORKS")}
    universe, selected = None, []
    if field_id in names:
        ui_name, constant = names[field_id]
        actual = _select_options(ui, ui_name)
        options = _literal_assignments(source).get(constant, [])
        valid_pairs = isinstance(options, (list, tuple)) and all(isinstance(item, (list, tuple)) and len(item) == 2 for item in options)
        selected = [item[0] for item in options] if valid_pairs else []
        if actual and options and valid_pairs:
            universe = actual
    markers = ["for index, (value, label) in enumerate(options)", "response.request.post_data_json == payload",
               "UI enum differs from declared contract", "reload changed enum metadata"]
    if not all(marker in source for marker in markers) or not _source_flow(source, "image"):
        universe, selected = None, []
    value_contracts = {str(value): [contract["id"] for contract in group] if value in selected else []
                       for value in (universe if universe is not None else selected)}
    return [_metric(group, "options-" + field_id, field_id + " 可选值", universe, selected,
                    f"分母来自 {ui_file} 对应 Form.Item 的实际 options；分子来自 {file} 的逐值选择/精确请求/回读断言。动态服务端字典无法离线确定时分母未知。仅登记覆盖。",
                    [file + ": " + marker for marker in markers], contract_map=value_contracts)]


def _token_enums(root, group):
    if group[0].get("test_file") != "test/e2e/platform_token_options.py":
        return []
    ui_file = "ui/src/pages/apikeys/PlatformTokensCard.tsx"
    ui = _text(root, ui_file)
    scopes = _select_options(ui, "scope")
    days_match = re.search(r"\bconst\s+EXPIRY_DAYS\s*=\s*(\[[^;]+\])", ui)
    try:
        days = ast.literal_eval(days_match[1]) if days_match else []
    except (ValueError, SyntaxError):
        days = []
    file = group[0]["test_file"]
    source = _text(root, file)
    cases = _literal_assignments(source).get("CASES", {})
    markers = ["response.request.post_data_json == expected_body", 'item["scope"] == scope', "expiry does not match selected UTC days"]
    flow_valid = all(marker in source for marker in markers) and _source_flow(source, "token")
    selected, contract_map, valid_contracts = [], {}, []
    for contract in group:
        selection = contract.get("selection", {})
        params = selection.get("parameters", {})
        pair = (params.get("scope"), params.get("days"))
        if tuple(cases.get(selection.get("case"), ())) == pair and flow_valid:
            key = str(pair[0]) + " × " + str(pair[1])
            selected.append(pair)
            valid_contracts.append(contract)
            contract_map.setdefault(key, []).append(contract["id"])
    basis = f"完整选项来自 {ui_file} 的 scope options 与 EXPIRY_DAYS；登记参数必须同时对应 {file} CASES 和精确请求/持久化断言。不是实际到期行为覆盖。"
    universe = [str(scope) + " × " + str(day) for scope in scopes for day in days] if scopes and days and flow_valid else None
    rows = [_metric(group, "scope-expiry", "平台令牌 scope × expiry 组合", universe,
                    [str(scope) + " × " + str(day) for scope, day in selected], basis,
                    [file + ": " + marker for marker in markers], "combination", contract_map)]
    for index, (name, full) in enumerate([("scope", scopes), ("expiry", days)]):
        rows.append(_metric(group, "options-" + name, "平台令牌 " + name + " 可选值", (full or None) if flow_valid else None,
                            [pair[index] for pair in selected], basis, rows[0]["assertions"], contract_map={
                                str(value): [c["id"] for c in valid_contracts if c.get("selection", {}).get("parameters", {}).get("scope" if index == 0 else "days") == value]
                                for value in full}))
    return rows


def _network_enums(root, group):
    details = group[0]["details"]
    dictionary = details.get("dictionary")
    if group[0].get("test_file") != "test/e2e/network_type_roles.py" or not isinstance(dictionary, dict):
        return []
    product_file = "internal/server/netplan/vocab.go"
    product, test = _text(root, product_file), _text(root, group[0]["test_file"])
    constants = dict(re.findall(r'\b(\w+)\s*=\s*"([^"]+)"', product))
    def go_array(name):
        match = re.search(r"var " + name + r" = \[\]string\{([^}]+)\}", product)
        return [constants.get(value.strip()) for value in match[1].split(",")] if match else []
    roles, types = go_array("Roles"), go_array("Types")
    # This adapter intentionally recognizes only the current pure dictionary shape.
    restriction = re.search(r"if t == (\w+) \{\s*return \[\]string\{(\w+)\}\s*\}\s*return append\(\[\]string\(nil\), Roles\.\.\.\)", product)
    product_roles = {kind: ([constants.get(restriction[2])] if kind == constants.get(restriction[1]) else roles)
                     for kind in types} if restriction else {}
    literals = _literal_assignments(test)
    role_assignment = _assignment(_parse(test), "TYPE_ROLES")
    test_roles = {}
    if role_assignment and isinstance(role_assignment.value, ast.Dict):
        for key, value in zip(role_assignment.value.keys, role_assignment.value.values):
            if isinstance(key, ast.Constant) and isinstance(key.value, str):
                try:
                    test_roles[key.value] = literals.get(value.id) if isinstance(value, ast.Name) else ast.literal_eval(value)
                except (ValueError, TypeError):
                    pass
    valid = (roles == dictionary.get("roles") and types == dictionary.get("types") and
             product_roles == dictionary.get("type_roles") == test_roles and literals.get("ROLES") == roles and literals.get("TYPES") == types)
    markers = ["COMBINATIONS[key]", "created = mutate(", "actual = persisted(", "to_be_checked(checked=role in roles)"]
    valid = valid and all(marker in test for marker in markers) and _source_flow(test, "network")
    basis = f"分母来自 {product_file} Roles/Types/TypeRoles 的全部合法非空子集；manifest dictionary 与产品/测试字典须一致，分子绑定实际叶合同及精确 POST、GET、刷新选中态断言。仅网络定义覆盖，不代表流量。"
    rows = []
    for kind, allowed in dictionary.get("type_roles", {}).items():
        if not isinstance(allowed, list):
            continue
        universe = [kind + "--" + "-".join(values) for size in range(1, len(allowed) + 1)
                    for values in itertools.combinations(allowed, size)]
        contract_map = {value: [contract["id"] for contract in group
                                if contract.get("selection", {}).get("case") == value
                                and contract.get("test_function") == "test_type_role_combination"] for value in universe}
        selected = [value for value, ids in contract_map.items() if ids] if valid else []
        row = _metric(group, "roles-" + kind, kind + " 非空角色组合", universe if valid else None, selected, basis,
                      [group[0]["test_file"] + ": " + marker for marker in markers], "combination", contract_map)
        rows.append(row)
    return rows


def _control_records(details):
    """Preserve separate inventory/control contexts instead of guessing field equivalence."""
    for section in ("fields", "components", "inventory", "controls", "other_controls", "other_reachable_controls"):
        controls = details.get(section, [])
        if isinstance(controls, dict):
            if section in {"other_controls", "other_reachable_controls"}:
                controls = [{"field_id": section, "component": "其他控件声明摘要", **controls}]
            else:
                controls = [{"field_id": key, **value} if isinstance(value, dict) else
                            {"field_id": key, "component": "未登记", "description": value}
                            for key, value in controls.items()]
        if not isinstance(controls, list):
            continue
        for index, control in enumerate(controls):
            if isinstance(control, str):
                control = {"id": section + ":" + control, "component": control, "description": control}
            if not isinstance(control, dict):
                continue
            explicit_id = control.get("field_id", control.get("id", control.get("field")))
            if explicit_id is not None:
                control_id = str(explicit_id)
            elif section == "inventory":
                fields = control.get("fields", [])
                control_id = "inventory:" + str(control.get("component", index)) + ":" + str(fields)
            else:
                control_id = section + ":" + str(control.get("component", index))
            yield section, index, control_id, control


def _fields(group):
    rows = {}
    for contract in group:
        for section, index, control_id, control in _control_records(contract.get("details", {})):
            context = f"/{section}/{index}"
            component_type = str(control.get("component", control.get("type", "未登记组件类型")))
            name = control_id if control_id.startswith("inventory:") else control_id + " · " + component_type
            if control_id not in rows:
                rows[control_id] = _row(group, "field-" + control_id, name, "field")
            row = rows[control_id]
            for key in ("covered", "missing", "not_applicable"):
                value = control.get(key, [])
                values = [str(item) for item in value] if isinstance(value, list) else [str(value)] if value else []
                row[key].extend(item for item in values if item not in row[key])
            declarations = [f"{context}/{key}: {control[key]}" for key in
                            ("request", "persistence", "persisted", "contract", "semantics", "execution",
                             "fields", "dimensions", "description", "reference", "inventory", "scope") if key in control]
            row["assertions"].extend(item for item in declarations if item not in row["assertions"])
            row["unknown"] = ["尚无该控件的完整检查项/有效枚举分母；自然语言声明条数不作为覆盖率分母"]
            row["basis"] = "控件声明明细（保留 fields/components/inventory 来源）；用例级规范项不会复制为每个同类型控件的覆盖。可选值/组合与规范项指标另行展示。"
    return list(rows.values())


def _enabled_filter(root, group):
    file = group[0].get("test_file")
    if file != "test/e2e/image_catalog_filters.py":
        return []
    controls = group[0].get("details", {}).get("components", [])
    control = next((item for item in controls if isinstance(item, dict)
                    and item.get("id") == "image-series.enabled-filter"), None)
    if not control:
        return []
    product_file = "ui/src/pages/images/ImageSeriesTable.tsx"
    product, test = _text(root, product_file), _text(root, file)
    block = re.search(r'key:\s*"enabled"(.*?)onFilter:', product, re.S)
    options = re.search(r"filters:\s*\[(.*?)\]", block[1], re.S) if block else None
    universe = re.findall(r'value:\s*"([^"]+)"', options[1]) if options else None
    markers = ['("enabled", status)', 'for option in options:',
               'select_filter(page, title, [option], options, locale)',
               'assert_rows(expected, f"{field}/option:{option}")',
               'status = [text["common"]["enabled"], text["common"]["disabled"]]']
    bound = all(marker in test for marker in markers) and _source_flow(test, "filter")
    selected = [value for value in control.get("dimensions", []) if value in {"enabled", "disabled"}] if bound else []
    if not bound:
        universe = None
    mapping = {str(value): [item["id"] for item in group] if value in selected else []
               for value in (universe or selected)}
    return [_metric(group, "filter-enabled-options", "image-series.enabled-filter 可选值", universe, selected,
                    f"分母来自 {product_file} enabled列的固定filter值；分子绑定 {file} 对每个状态独立选择、精确行集合、重置和权威GET断言。其余数据驱动筛选分母仍未知。仅登记覆盖。",
                    [file + ": " + marker for marker in markers], contract_map=mapping)]


def components(root: Path, data: dict) -> list[dict]:
    """Produce conservative component/checklist/enum views from canonical declarations."""
    root = root.resolve()
    sections = _sections(root)
    groups = {}
    for contract in data.get("contracts", []):
        if contract.get("kind") == "business" and not contract.get("conflict"):
            groups.setdefault((contract["manifest"], contract["module"]), []).append(contract)
    rows = []
    for group in groups.values():
        rows.extend(_checklists(group, sections))
        rows.extend(_fields(group))
        rows.extend(_image_enums(root, group))
        rows.extend(_enabled_filter(root, group))
        rows.extend(_token_enums(root, group))
        rows.extend(_network_enums(root, group))
    rows.extend(value_rows(root, data, _metric))
    return rows
