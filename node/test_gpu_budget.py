import unittest
from pathlib import Path
from google.protobuf import descriptor_pb2, descriptor_pool, message_factory
from ember_pb2 import ResourceProfile, GPUBudget
from agent.gpu_budget import declared_budget
class GPUBudgetTests(unittest.TestCase):
    def test_contract_values(self):
        b=declared_budget({'accelerator_memory_bytes':24000},object(),{'EMBER_GPU_MEMORY_FRACTION':'.5','EMBER_GPU_COMPUTE_CAP':'.25'})
        self.assertEqual((b.memory_permille,b.compute_permille,b.accelerator_memory_bytes),(500,250,24000))
        self.assertIsNone(declared_budget({},object(),{}))
        with self.assertRaises(ValueError):declared_budget({},object(),{'EMBER_GPU_MEMORY_FRACTION':'.5'})
        with self.assertRaises(ValueError):declared_budget({'accelerator_memory_bytes':1},object(),{'EMBER_GPU_MEMORY_FRACTION':'.0001'})
    def test_old_new_wire_bidirectional(self):
        desc=descriptor_pb2.FileDescriptorSet.FromString((Path(__file__).parents[1]/'testdata/resource-profile-pre-gpu.desc').read_bytes())
        pool=descriptor_pool.DescriptorPool()
        for d in desc.file:pool.Add(d)
        Old=message_factory.GetMessageClass(pool.FindMessageTypeByName('ember.ResourceProfile'))
        old=Old(node_id='old',chain_id='model');new=ResourceProfile.FromString(old.SerializeToString())
        self.assertFalse(new.HasField('gpu_budget'))
        new.gpu_budget.CopyFrom(GPUBudget(memory_permille=500,compute_permille=250,accelerator_memory_bytes=24000))
        reread=Old.FromString(new.SerializeToString());self.assertEqual(reread.node_id,'old')
        restored=ResourceProfile.FromString(reread.SerializeToString());self.assertEqual(restored.gpu_budget,new.gpu_budget)
