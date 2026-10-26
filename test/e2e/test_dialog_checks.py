"""Pure geometry for dialog section 12. Run: python3 -m unittest test/e2e/test_dialog_checks.py"""
import unittest

from dialog_checks import boxes_stacked, in_viewport, regions_stacked


def box(top, left, bottom, right):
    return {"top": top, "left": left, "bottom": bottom, "right": right}


class BoxesStackedTest(unittest.TestCase):
    def test_vertical_stack_inside_dialog(self):
        outer = box(10, 10, 200, 300)
        self.assertTrue(boxes_stacked(box(20, 20, 40, 280), box(40, 20, 150, 280), box(150, 20, 190, 280), outer))

    def test_title_overlapping_body_fails(self):
        outer = box(0, 0, 100, 100)
        self.assertFalse(boxes_stacked(box(0, 0, 30, 100), box(20, 0, 70, 100), box(70, 0, 100, 100), outer))

    def test_region_outside_dialog_fails(self):
        self.assertFalse(boxes_stacked(box(0, 0, 10, 10), box(10, 0, 20, 10), box(20, 0, 30, 10), box(0, 0, 15, 10)))

    def test_missing_region_fails(self):
        self.assertFalse(boxes_stacked(None, box(1, 1, 2, 2), box(2, 1, 3, 2), box(0, 0, 4, 4)))


class DrawerLayoutTest(unittest.TestCase):
    def test_four_regions_stack_inside_the_drawer(self):
        outer = box(0, 0, 900, 640)
        ordered = (box(0, 0, 56, 640), box(80, 24, 160, 600), box(180, 24, 700, 600), box(720, 24, 752, 120))
        self.assertTrue(regions_stacked(ordered, outer))
        self.assertTrue(in_viewport(ordered[3], {"width": 1440, "height": 900}))

    def test_header_overlapping_description_fails(self):
        outer = box(0, 0, 900, 640)
        ordered = (box(0, 0, 80, 640), box(40, 24, 140, 600), box(160, 24, 400, 600), box(420, 24, 452, 120))
        self.assertFalse(regions_stacked(ordered, outer))

    def test_action_outside_the_drawer_fails(self):
        outer = box(0, 0, 900, 640)
        ordered = (box(0, 0, 56, 640), box(80, 24, 160, 600), box(180, 24, 860, 600), box(880, 24, 940, 120))
        self.assertFalse(regions_stacked(ordered, outer))
        self.assertFalse(in_viewport(ordered[3], {"width": 1440, "height": 900}))


if __name__ == "__main__":
    unittest.main()
