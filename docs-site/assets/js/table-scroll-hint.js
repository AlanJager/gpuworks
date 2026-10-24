document.querySelectorAll('.td-content .td-table-scroll').forEach(function (wrapper) {
  var table = wrapper.querySelector('table');
  if (!table) return;
  var hint = document.createElement('div');
  hint.className = 'gw-table-hint';
  hint.textContent = document.documentElement.lang.startsWith('en')
    ? 'Scroll horizontally to view the full table ↔'
    : '左右滚动查看完整表格 ↔';
  hint.hidden = true;
  hint.setAttribute('aria-hidden', 'true');
  wrapper.before(hint);
  var observer = new ResizeObserver(function () {
    hint.hidden = wrapper.scrollWidth <= wrapper.clientWidth + 1;
  });
  observer.observe(wrapper);
  observer.observe(table);
});
