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
