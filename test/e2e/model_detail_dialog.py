def model_detail_dialog(page, labels, name):
    return page.get_by_role("dialog", name=labels["detail"]["title"], exact=True).filter(
        has=page.get_by_role("heading", name=name, exact=True, level=4))
