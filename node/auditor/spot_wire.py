"""Auditor-local v14 protobuf compatibility until the chain schema is merged.

Field numbers match train/2026-11-19-v14-a. Never alter the shared generated
schema on this line. A private descriptor uses ordinary protobuf serialization
and signatures, including with the pre-v14 installed generated module.
"""
import ember_pb2
from google.protobuf import descriptor_pb2, descriptor_pool, message_factory


def verdict_type():
    if 'per_step' in ember_pb2.AuditVerdict.DESCRIPTOR.fields_by_name:
        return ember_pb2.AuditVerdict
    schema = descriptor_pb2.FileDescriptorProto.FromString(ember_pb2.DESCRIPTOR.serialized_pb)
    step = schema.message_type.add(name='SpotStepResult')
    step.field.add(name='step', number=1, type=4, label=1)
    step.field.add(name='outcome', number=2, type=9, label=1)
    step.field.add(name='first_divergence_distance', number=3, type=1, label=1)
    verdict = next(m for m in schema.message_type if m.name == 'AuditVerdict')
    verdict.field.add(name='per_step', number=14, type=11, label=3,
                      type_name='.' + schema.package + '.SpotStepResult')
    verdict.field.add(name='verdict_distance', number=15, type=1, label=1)
    verdict.field.add(name='verdict_kind', number=16, type=9, label=1)
    pool = descriptor_pool.DescriptorPool()
    pool.Add(schema)
    return message_factory.GetMessageClass(pool.FindMessageTypeByName(schema.package + '.AuditVerdict'))


SpotVerdict = verdict_type()
