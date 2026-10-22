package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// escapeCell makes text safe inside a single markdown table cell: no
// literal pipes (they would split the cell) and no embedded newlines.
func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.TrimSpace(s)
}

// relLink returns the path to open relPath (repo-root-relative) from a
// markdown file that lives in fromDir (also repo-root-relative, "" for the
// repo root itself), as a forward-slashed relative link.
func relLink(fromDir, relPath string) (string, error) {
	rel, err := filepath.Rel(fromDir, relPath)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// outcomeStatuses are the terminal-ish statuses whose table cell prefers the
// one-line outcome (what was delivered / not done / why dropped) over the
// summary, per CLAUDE.md. active/proposed always show summary, since there
// is no outcome yet.
var outcomeStatuses = map[string]bool{"done": true, "dropped": true, "superseded": true}

// summaryCellText picks the text a doc's table cell should show: its
// outcome when the status prefers one and it is set, falling back to the
// summary otherwise.
func summaryCellText(d Doc) string {
	if outcomeStatuses[d.Status] && strings.TrimSpace(d.Outcome) != "" {
		return d.Outcome
	}
	return d.Summary
}

// sortTopicsByDirDesc sorts topics by their date-prefixed directory name,
// newest first, breaking ties by full path for determinism.
func sortTopicsByDirDesc(topics []Topic) {
	sort.SliceStable(topics, func(i, j int) bool {
		if topics[i].Dir != topics[j].Dir {
			return topics[i].Dir > topics[j].Dir
		}
		return false
	})
}

const topicTableHeader = "| 主题目录 | 标题 | status | created | updated | 摘要 |\n|---|---|---|---|---|---|\n"

// topicRow renders one topic as a table row, linking to its entry document
// relative to fromDir (repo-root-relative, "" for the repo root).
func topicRow(fromDir string, t Topic) (string, error) {
	link, err := relLink(fromDir, t.Entry.RelPath)
	if err != nil {
		return "", err
	}
	dirLink, err := relLink(fromDir, t.Dir+"/")
	if err != nil {
		return "", err
	}
	summary := escapeCell(summaryCellText(t.Entry))
	return fmt.Sprintf("| [%s](%s) | [%s](%s) | %s | %s | %s | %s |\n",
		escapeCell(t.Dir), dirLink, escapeCell(t.Entry.Title), link, t.Entry.Status, t.Entry.Created, t.Entry.Updated, summary), nil
}

func renderTopicTable(fromDir string, topics []Topic) (string, error) {
	var b strings.Builder
	b.WriteString(topicTableHeader)
	for _, t := range topics {
		row, err := topicRow(fromDir, t)
		if err != nil {
			return "", err
		}
		b.WriteString(row)
	}
	return b.String(), nil
}

const artifactsReadmeNote = `# artifacts/ 索引

本文件由工具生成，**不要手改**：每个主题目录的状态、日期、摘要只写在其入口文件（` + "`README.md`" + `
或 ` + "`plan.md`" + `/` + "`plan-vN.md`" + `）自身的 YAML frontmatter 里，这里只是按 frontmatter 汇总的索引。
改完某个入口文件的 frontmatter，或新增/关闭一个主题后运行：

` + "```" + `
go run ./scripts/artifactsindex -write
` + "```" + `

这会重新生成本文件、` + "`artifacts/.ignore`" + `（列出所有 superseded/dropped 文档，供 ` + "`rg`" + ` 默认跳过）
和 ` + "`artifacts/plan.md`" + ` 里的生成区块（命令在主仓库根目录运行；本目录是独立的私有仓库，检出在主仓库的 ` + "`artifacts/`" + ` 下）。` + "`make check-artifacts`" + ` 验证三者与 frontmatter 是否一致，并检查每个
主题目录恰有一个入口文件、` + "`work/`" + ` 主题入口是 active/proposed、` + "`archive/`" + `与` + "`research/`" + `下的
文档都不是 active/proposed。

目录约定：` + "`work/<YYYY-MM-DD>-<主题>/`" + ` 进行中，` + "`archive/<YYYY-MM-DD>-<主题>/`" + ` 已收口
（同一主题关闭时只把目录从 ` + "`work/`" + ` 搬进 ` + "`archive/`" + `，目录名不变），
` + "`research/<YYYY-MM-DD>-<主题>/`" + ` 是独立完成的调研，` + "`archive/todo-resolved/<YYYY-MM>.md`" + ` 是
已解决 TODO 的按月归档（不是主题目录，没有 frontmatter）。
`

// generateArtifactsReadme renders the full content of artifacts/README.md:
// the note above, then three tables (work, research, archive-by-date-desc).
func generateArtifactsReadme(topics []Topic) (string, error) {
	fromDir := "artifacts"
	var work, research, archive []Topic
	for _, t := range topics {
		switch t.Base {
		case "artifacts/work":
			work = append(work, t)
		case "artifacts/research":
			research = append(research, t)
		case "artifacts/archive":
			archive = append(archive, t)
		}
	}
	sortTopicsByDirDesc(work)
	sortTopicsByDirDesc(research)
	sortTopicsByDirDesc(archive)

	var b strings.Builder
	b.WriteString(artifactsReadmeNote)

	b.WriteString("\n## 进行中（work）\n\n")
	if len(work) == 0 {
		b.WriteString("（当前没有进行中的主题。）\n")
	} else {
		table, err := renderTopicTable(fromDir, work)
		if err != nil {
			return "", err
		}
		b.WriteString(table)
	}

	b.WriteString("\n## 调研（research）\n\n")
	if len(research) == 0 {
		b.WriteString("（当前没有独立调研主题。）\n")
	} else {
		table, err := renderTopicTable(fromDir, research)
		if err != nil {
			return "", err
		}
		b.WriteString(table)
	}

	b.WriteString("\n## 归档（archive，按目录日期倒序）\n\n")
	if len(archive) == 0 {
		b.WriteString("（当前没有已归档主题。）\n")
	} else {
		table, err := renderTopicTable(fromDir, archive)
		if err != nil {
			return "", err
		}
		b.WriteString(table)
	}
	return b.String(), nil
}

const ignoreFileHeader = `# 本文件由 ` + "`go run ./scripts/artifactsindex -write`" + ` 生成，**不要手改**。
#
# 列出 artifacts/ 下所有 status 为 superseded 或 dropped 的文档（路径相对本文件所在
# 目录，即相对 artifacts/）。ripgrep（rg）默认会跳过同目录下名为 .ignore 的文件里列出
# 的路径，所以已被取代/放弃的历史计划稿不会污染 rg 的默认搜索结果，仍可用
# ` + "`rg --no-ignore`" + ` 显式搜索到。` + "`make check-artifacts`" + ` 校验本文件是最新的。
`

// generateIgnoreFile renders artifacts/.ignore: the header above, then every
// superseded/dropped document's path relative to artifacts/, sorted.
func generateIgnoreFile(docs []Doc) (string, error) {
	var rels []string
	for _, d := range docs {
		if d.Status != "superseded" && d.Status != "dropped" {
			continue
		}
		rel, err := relLink("artifacts", d.RelPath)
		if err != nil {
			return "", err
		}
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	var b strings.Builder
	b.WriteString(ignoreFileHeader)
	for _, r := range rels {
		b.WriteString(r)
		b.WriteString("\n")
	}
	return b.String(), nil
}

const genBeginMarker = "<!-- BEGIN GENERATED: active-plans -->"
const genEndMarker = "<!-- END GENERATED: active-plans -->"

// generatePlanSection renders the content that belongs between the
// BEGIN/END GENERATED markers in artifacts/plan.md: one row per
// artifacts/work/ topic (not per document — a topic like a100-verification
// bundles several documents behind one entry), links resolved relative to
// the repo root.
func generatePlanSection(topics []Topic) (string, error) {
	var work []Topic
	for _, t := range topics {
		if t.Base == "artifacts/work" {
			work = append(work, t)
		}
	}
	sortTopicsByDirDesc(work)

	var b strings.Builder
	b.WriteString("\n")
	if len(work) == 0 {
		b.WriteString("（当前没有进行中的主题；已收口的主题见 `artifacts/README.md` 的归档表。）\n\n")
		return b.String(), nil
	}
	table, err := renderTopicTable("", work)
	if err != nil {
		return "", err
	}
	b.WriteString(table)
	b.WriteString("\n")
	return b.String(), nil
}

// spliceGenerated replaces the content strictly between genBeginMarker and
// genEndMarker in content with generated (which must not itself contain the
// markers). It returns an error if the markers are missing, unbalanced, or
// out of order — -write must not guess where to put a new marker pair; that
// is a one-time manual edit (see CLAUDE.md).
func spliceGenerated(content, generated string) (string, error) {
	beginIdx := strings.Index(content, genBeginMarker)
	if beginIdx < 0 {
		return "", fmt.Errorf("artifacts/plan.md is missing %q; add the marker pair manually once (see CLAUDE.md)", genBeginMarker)
	}
	afterBegin := beginIdx + len(genBeginMarker)
	endIdx := strings.Index(content[afterBegin:], genEndMarker)
	if endIdx < 0 {
		return "", fmt.Errorf("artifacts/plan.md is missing %q after %q", genEndMarker, genBeginMarker)
	}
	endIdx += afterBegin
	return content[:afterBegin] + generated + content[endIdx:], nil
}
