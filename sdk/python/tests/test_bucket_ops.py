#
# Copyright (c) 2018-2022, NVIDIA CORPORATION. All rights reserved.
#

# Default provider is AIS, so all Cloud-related tests are skipped.

import random
import string
import unittest
from aistore.client.errors import ErrBckNotFound
import tempfile

from aistore.client.api import Client
import requests
from . import CLUSTER_ENDPOINT, REMOTE_BUCKET


class TestObjectOps(unittest.TestCase):  # pylint: disable=unused-variable
    def setUp(self) -> None:
        letters = string.ascii_lowercase
        self.bck_name = ''.join(random.choice(letters) for _ in range(10))

        self.client = Client(CLUSTER_ENDPOINT)
        self.buckets = []

    def tearDown(self) -> None:
        # Try to destroy all temporary buckets if there are left.
        for bck_name in self.buckets:
            try:
                self.client.bucket(bck_name).delete()
            except ErrBckNotFound:
                pass

    def test_bucket(self):
        res = self.client.cluster().list_buckets()
        count = len(res)
        self.create_bucket(self.bck_name)
        res = self.client.cluster().list_buckets()
        count_new = len(res)
        self.assertEqual(count + 1, count_new)

    def create_bucket(self, bck_name):
        self.buckets.append(bck_name)
        self.client.bucket(bck_name).create()

    def test_head_bucket(self):
        self.create_bucket(self.bck_name)
        self.client.bucket(self.bck_name).head()
        self.client.bucket(self.bck_name).delete()
        try:
            self.client.bucket(self.bck_name).head()
        except requests.exceptions.HTTPError as e:
            self.assertEqual(e.response.status_code, 404)

    def test_rename_bucket(self):
        from_bck_n = self.bck_name + 'from'
        to_bck_n = self.bck_name + 'to'
        self.create_bucket(from_bck_n)
        res = self.client.cluster().list_buckets()
        count = len(res)
        # wait for rename to finish
        xact_id = self.client.bucket(from_bck_n).rename(to_bck=to_bck_n)
        self.assertNotEqual(xact_id, "")
        self.client.wait_for_xaction_finished(xact_id=xact_id)
        # new bucket should be created and accessible
        self.client.bucket(to_bck_n).head()
        # old bucket should be inaccessible
        try:
            self.client.bucket(from_bck_n).head()
        except requests.exceptions.HTTPError as e:
            self.assertEqual(e.response.status_code, 404)
        # length of buckets before and after rename should be same
        res = self.client.cluster().list_buckets()
        count_new = len(res)
        self.assertEqual(count, count_new)

    @unittest.skipIf(REMOTE_BUCKET == "" or REMOTE_BUCKET.startswith("ais:"), "Remote bucket is not set")
    def test_evict_bucket(self):
        obj_name = "evict_obj"
        parts = REMOTE_BUCKET.split("://")  # must be in the format '<provider>://<bck>'
        self.assertTrue(len(parts) > 1)
        provider, self.bck_name = parts[0], parts[1]
        content = "test".encode("utf-8")
        with tempfile.NamedTemporaryFile() as f:
            f.write(content)
            f.flush()
            self.client.bucket(self.bck_name, provider=provider).object(obj_name).put(f.name)

        objects = self.client.bucket(self.bck_name, provider=provider).list_objects(props="name,cached", prefix=obj_name)
        self.assertTrue(len(objects) > 0)
        for obj in objects:
            if obj.name == obj_name:
                self.assertTrue(obj.is_ok())
                self.assertTrue(obj.is_cached())

        self.client.bucket(self.bck_name, provider=provider).evict()
        objects = self.client.bucket(self.bck_name, provider=provider).list_objects(props="name,cached", prefix=obj_name)
        self.assertTrue(len(objects) > 0)
        for obj in objects:
            if obj.name == obj_name:
                self.assertTrue(obj.is_ok())
                self.assertFalse(obj.is_cached())
        self.client.bucket(self.bck_name, provider=provider).object(obj_name).delete()

    def test_copy_bucket(self):
        from_bck = self.bck_name + 'from'
        to_bck = self.bck_name + 'to'
        self.create_bucket(from_bck)
        self.create_bucket(to_bck)

        xact_id = self.client.bucket(from_bck).copy(to_bck)
        self.assertNotEqual(xact_id, "")
        self.client.wait_for_xaction_finished(xact_id=xact_id)


if __name__ == '__main__':
    unittest.main()
