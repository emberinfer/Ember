"""Installer runtime selection must fail before copying into the prefix."""
import os
from pathlib import Path
import subprocess

ROOT=Path(__file__).resolve().parents[1]


def run(tmp_path, family, arch='x86_64', gpu=True, driver='580.105.08'):
    commands=tmp_path/'commands';commands.mkdir()
    for name,body in {'uname':f'echo {arch}', 'nvidia-smi':('exit 1' if not gpu else f'if [ "$1" = -L ]; then echo "GPU 0: Test NVIDIA"; else echo {driver}; fi')}.items():
        p=commands/name;p.write_text('#!/bin/sh\n'+body+'\n');p.chmod(0o755)
    prefix=tmp_path/'prefix'
    env=dict(os.environ,PATH=str(commands)+':'+os.environ['PATH'])
    result=subprocess.run(['bash',str(ROOT/'deploy/install-release.sh'),'--role','node','--prefix',str(prefix),'--profile-family',family,'--python','s4-nonexistent-python'],env=env,capture_output=True,text=True)
    return result,prefix


def test_cuda_arm64_rejected_before_copy(tmp_path):
    result,prefix=run(tmp_path,'cuda',arch='aarch64')
    assert result.returncode and 'x86_64 only' in result.stderr and not prefix.exists()


def test_cuda_no_gpu_rejected_before_copy(tmp_path):
    result,prefix=run(tmp_path,'cuda',gpu=False)
    assert result.returncode and 'working NVIDIA GPU' in result.stderr and not prefix.exists()


def test_cuda_old_driver_rejected_before_copy(tmp_path):
    result,prefix=run(tmp_path,'cuda',driver='535.104.05')
    assert result.returncode and '>=550.54.14' in result.stderr and not prefix.exists()


def test_cpu_gpu_notice_does_not_switch_family(tmp_path):
    result,prefix=run(tmp_path,'cpu')
    assert 'CPU family remains selected' in result.stderr
    assert 'Python 3.12 is required' in result.stderr and not prefix.exists()


def test_real_venv_probe_failure_leaves_no_prefix(tmp_path):
    import sys
    commands=tmp_path/'commands';commands.mkdir()
    wrapper=commands/'python3.12'
    wrapper.write_text('#!/bin/sh\nexec '+sys.executable+''' -c 'import sys,venv; venv.EnvBuilder.create=lambda *a,**kw: (_ for _ in ()).throw(RuntimeError("ensurepip is disabled")); exec(sys.stdin.read())' "$@"
''')
    wrapper.chmod(0o755)
    prefix=tmp_path/'prefix'
    result=subprocess.run(['bash',str(ROOT/'deploy/install-release.sh'),'--role','node','--prefix',str(prefix)],env=dict(os.environ,PATH=str(commands)+':'+os.environ['PATH']),capture_output=True,text=True)
    assert result.returncode
    assert 'sudo apt-get install -y python3.12-venv' in result.stderr
    assert 'ensurepip is disabled' in result.stderr
    assert not prefix.exists()


def test_normal_probe_and_cpu_install_pass(tmp_path):
    import shutil
    import sys
    bundle=tmp_path/'bundle'
    for name in ('bin','lib/ember','profiles','docs','config'):
        (bundle/name).mkdir(parents=True,exist_ok=True)
    for name in ('requirements-cpu.txt','requirements-spi.txt'):
        (bundle/'lib/ember'/name).write_text('# Empty requirements for installer control-flow test\n')
    for name in ('node.env.example','trust-policy.json'):
        (bundle/'config'/name).write_text('{}')
    (bundle/'network.json').write_text('{}')
    (bundle/'ember-role').write_text('#!/bin/sh\n')
    shutil.copy(ROOT/'deploy/install-release.sh',bundle/'install-release.sh')
    prefix=tmp_path/'prefix'
    result=subprocess.run(['bash',str(bundle/'install-release.sh'),'--role','node','--prefix',str(prefix),'--python',sys.executable],capture_output=True,text=True)
    assert result.returncode==0, result.stderr
    assert (prefix/'venv/bin/pip').exists()
