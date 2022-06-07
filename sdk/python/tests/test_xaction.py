#
# Copyright (c) 2018-2022, NVIDIA CORPORATION. All rights reserved.
#

# Default provider is AIS, so all Cloud-related tests are skipped.

import random
import string
import unittest
import requests

from aistore.client.api import Client
from . import CLUSTER_ENDPOINT


class TestObjectOps(unittest.TestCase):  # pylint: disable=unused-variable
    def setUp(self) -> None:
        letters = string.ascii_lowercase
        self.bck_name = ''.join(random.choice(letters) for _ in range(10))

        self.client = Client(CLUSTER_ENDPOINT)

    def tearDown(self) -> None:
        # Try to destroy bucket if there is one left.
        try:
            self.client.destroy_bucket(self.bck_name)
        except requests.exceptions.HTTPError:
            pass

    def test_xaction_start(self):
        self.client.create_bucket(self.bck_name)
        xact_id = self.client.xact_start(xact_kind="lru")
        self.client.wait_for_xaction_finished(xact_id=xact_id)


if __name__ == '__main__':
    unittest.main()
