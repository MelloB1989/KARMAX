#!/usr/bin/env python3
"""Tests for ids.normalize_id(). Run directly: python3 scripts/test_ids.py

Exists because a regression here is the worst failure mode this skill can
produce — not a duplicate or a drop, but a DM sent to the wrong account.
See ids.py's own docstring for the incident this guards against: a real
17-digit id round-tripped through float() came back as a different real
id, and that corrupted value is what would have gone out on the wire.
"""

import sys

from ids import normalize_id


def test_string_and_int_17_digit_ids_produce_identical_exact_digits():
    as_str = normalize_id("17841406338528803")
    as_int = normalize_id(17841406338528803)
    assert as_str == "17841406338528803", as_str
    assert as_int == "17841406338528803", as_int
    assert as_str == as_int


def test_adjacent_large_ids_stay_distinct_as_strings():
    # The exact regression: these two differ by 1 and both survived
    # float() as the same value under the old implementation.
    a = normalize_id("9007199254740993")
    b = normalize_id("9007199254740992")
    assert a == "9007199254740993", a
    assert b == "9007199254740992", b
    assert a != b


def test_string_never_round_trips_through_float_or_int():
    # A string id is used AS-IS; parsing it is exactly the bug.
    assert normalize_id("17841406338528803") == "17841406338528803"
    assert normalize_id("007") == "007"  # leading zeros preserved, not collapsed


def test_int_exact_at_any_size():
    huge = 123456789012345678901234567890
    assert normalize_id(huge) == str(huge)


def test_integral_float_in_exact_range_normalizes():
    assert normalize_id(1001.0) == "1001"
    assert normalize_id(0.0) == "0"


def test_non_integral_float_rejected():
    try:
        normalize_id(1001.5)
    except ValueError:
        pass
    else:
        raise AssertionError("expected ValueError for a non-integral float")


def test_float_outside_exact_integer_range_rejected():
    try:
        normalize_id(float(2**60))
    except ValueError:
        pass
    else:
        raise AssertionError("expected ValueError for a float outside the exact-integer range")


def test_bool_rejected():
    for v in (True, False):
        try:
            normalize_id(v)
        except ValueError:
            pass
        else:
            raise AssertionError(f"expected ValueError for bool {v!r}")


def test_dict_and_list_rejected():
    for v in ({"a": 1}, [1, 2, 3], None):
        try:
            normalize_id(v)
        except ValueError:
            pass
        else:
            raise AssertionError(f"expected ValueError for {v!r}")


def test_empty_string_rejected():
    try:
        normalize_id("   ")
    except ValueError:
        pass
    else:
        raise AssertionError("expected ValueError for an empty/whitespace string")


def main() -> int:
    tests = [v for k, v in sorted(globals().items()) if k.startswith("test_") and callable(v)]
    for t in tests:
        t()
        print(f"ok  {t.__name__}")
    print(f"{len(tests)} passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
