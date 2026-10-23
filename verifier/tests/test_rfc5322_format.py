import os
import tempfile
import unittest

from verifier.props import rfc5322_format

GOOD = (b"DKIM-Signature: v=1; a=rsa-sha256; c=relaxed/relaxed; d=auth-good.test; s=lab;\r\n"
        b" h=From:To:Subject; bh=AAAA; b=BBBB\r\n"
        b"Date: Fri, 23 Oct 2026 12:00:00 +0000\r\n"
        b"From: notify@auth-good.test\r\nTo: user1@outlook-model.test\r\nSubject: Hi\r\n"
        b"Message-ID: <m00000001.mtaproof@auth-good.test>\r\nMIME-Version: 1.0\r\n"
        b"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n"
        b"\r\nHello.\r\n")
# The header section runs into the body: no empty line separates them.
NO_SEPARATOR = (b"Date: Fri, 23 Oct 2026 12:00:00 +0000\r\nFrom: notify@auth-good.test\r\n"
                b"Subject: Hi\r\nHello, this body line is not a header field.\r\n")


class RFC5322FormatTest(unittest.TestCase):
    def write(self, root, run, name, data):
        d = os.path.join(root, run, "messages")
        os.makedirs(d, exist_ok=True)
        path = os.path.join(d, name)
        with open(path, "wb") as f:
            f.write(data)

    def test_clean_messages_pass(self):
        with tempfile.TemporaryDirectory() as root:
            self.write(root, "auth-1", "a.eml", GOOD)
            self.write(root, "lab-1/campaign", "b.eml", GOOD.replace(b"Hi", b"Hey"))
            os.makedirs(os.path.join(root, "other"))
            with open(os.path.join(root, "other", "c.eml"), "wb") as f:
                f.write(NO_SEPARATOR)  # not under messages/: not a capture
            paths = rfc5322_format.captured(root)
            ok, details, failures = rfc5322_format.check(paths, lambda p: os.path.relpath(p, root))
        self.assertTrue(ok, failures)
        self.assertEqual(details["files"], 2)
        self.assertEqual(details["runs"], ["auth-1", os.path.join("lab-1", "campaign")])

    def test_missing_header_separator_fails(self):
        with tempfile.TemporaryDirectory() as root:
            self.write(root, "auth-1", "a.eml", GOOD)
            self.write(root, "auth-1", "bad.eml", NO_SEPARATOR)
            ok, details, failures = rfc5322_format.check(rfc5322_format.captured(root))
        self.assertFalse(ok)
        self.assertEqual(details["with_defects"], 1)
        self.assertIn("MissingHeaderBodySeparatorDefect", failures[0])

    def test_defect_in_a_part_fails(self):
        # A multipart message whose boundary never appears: StartBoundaryNotFoundDefect.
        msg = (b"From: a@b.test\r\nDate: Fri, 23 Oct 2026 12:00:00 +0000\r\nMIME-Version: 1.0\r\n"
               b"Content-Type: multipart/mixed; boundary=xyz\r\n\r\nno parts here\r\n")
        self.assertIsNotNone(rfc5322_format.defects_of(msg))
        self.assertIsNone(rfc5322_format.defects_of(GOOD))

    def test_no_captures_fails(self):
        with tempfile.TemporaryDirectory() as root:
            ok, _, failures = rfc5322_format.check(rfc5322_format.captured(root))
        self.assertFalse(ok)
        self.assertEqual(failures, ["no captured .eml files"])


if __name__ == "__main__":
    unittest.main()
