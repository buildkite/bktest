"""Tests run while a class decorator or a test decorator freezes time."""

import unittest

from freezegun import freeze_time


@freeze_time("2001-01-01")
class TestFrozenClass(unittest.TestCase):
    def test_class_first(self):
        pass

    def test_class_second(self):
        pass


@freeze_time("2001-01-01")
def test_frozen_test():
    pass
