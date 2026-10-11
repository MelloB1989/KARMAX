import importlib.util
import pathlib
import unittest

spec = importlib.util.spec_from_file_location("make_edid", pathlib.Path(__file__).with_name("make-edid.py"))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


class EdidTest(unittest.TestCase):
    def setUp(self):
        self.b = m.make_edid(1920, 1080, 60)

    def test_block_is_128_bytes_with_a_valid_checksum(self):
        self.assertEqual(len(self.b), 128)
        self.assertEqual(sum(self.b) % 256, 0)

    def test_header_and_version(self):
        self.assertEqual(self.b[:8], bytes([0, 255, 255, 255, 255, 255, 255, 0]))
        self.assertEqual((self.b[18], self.b[19]), (1, 3))

    def test_preferred_timing_is_the_requested_mode(self):
        d = self.b[54:72]
        self.assertEqual(d[2] | ((d[4] >> 4) << 8), 1920)
        self.assertEqual(d[5] | ((d[7] >> 4) << 8), 1080)
        clock_10khz = d[0] | (d[1] << 8)
        self.assertEqual(clock_10khz, 13850)  # CVT-RB 1920x1080@60 is 138.5 MHz
        h_total = 1920 + (d[3] | ((d[4] & 0xF) << 8))
        v_total = 1080 + (d[6] | ((d[7] & 0xF) << 8))
        self.assertAlmostEqual(clock_10khz * 10_000 / (h_total * v_total), 60, delta=0.1)

    def test_name_descriptor(self):
        self.assertEqual(self.b[75], 0xFC)
        self.assertTrue(self.b[77:90].startswith(b"FLEET-VIRT"))

    def test_other_modes_keep_the_checksum(self):
        for w, h, hz in [(1280, 720, 60), (2560, 1440, 60), (1024, 768, 60)]:
            self.assertEqual(sum(m.make_edid(w, h, hz)) % 256, 0, (w, h, hz))

    def test_rejects_an_unsupported_mode(self):
        with self.assertRaises(ValueError):
            m.make_edid(100, 100, 60)


if __name__ == "__main__":
    unittest.main()
