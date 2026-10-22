# 文档站本地预览

使用与 CI 一致的 Hugo extended 0.165.0，在仓库根目录运行：

```bash
HUGO_BIN=/path/to/hugo bash docs-site/preview.sh
```

打开 <http://127.0.0.1:1313/docs/how-to/>。首次运行会按 `build.sh` 获取 OINK 主题；本地已有的主题可放在 `docs-site/themes/oink/`。端口被占用时设置 `DOCS_PREVIEW_PORT`。

操作指南的分类、导航标签和顺序来自 `docs/README.md` 的操作指南清单。修改文档或清单后，在另一个终端运行 `python3 docs-site/stage_docs.py`，Hugo 会重新加载生成内容；不要手工修改 `docs-site/content/`。

侧栏使用统一文档树，可以从任何页面直接展开“操作指南 → 分类 → 文章”，也可切换到教程、参考等栏目。检查目录页的四组及文章顺序，展开侧栏各组，进入文章确认所属组展开、当前项高亮，并检查链接仍使用原来的文章地址。提取和暂存回归检查：

```bash
python3 -m unittest discover -s docs-site -p 'test_*.py'
```
