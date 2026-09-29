import importlib.util
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('release_deploy', Path(__file__).with_name('deploy.py'))
deploy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deploy)


class DeployTests(unittest.TestCase):
    def test_http_200_degraded_is_not_ready(self):
        for payload in ({'status':'degraded'}, {'status':'ok','writes':{'status':'degraded'}}):
            response=io.BytesIO(json.dumps(payload).encode()); response.status=200
            def opened(*args, **kwargs):
                value=io.BytesIO(json.dumps(payload).encode()); value.status=200; return value
            with patch.object(deploy.subprocess,'run',return_value=SimpleNamespace(returncode=0)), patch.object(deploy.urllib.request,'urlopen',side_effect=opened), patch.object(deploy.time,'sleep'):
                self.assertFalse(deploy.healthy())

    def test_failed_release_restores_previous_and_verifies_it(self):
        with tempfile.TemporaryDirectory() as directory:
            base=Path(directory); releases=base/'releases'; releases.mkdir()
            old=releases/('a'*40); old.mkdir()
            current=base/'current'; current.symlink_to(old)
            archive=io.BytesIO()
            with tarfile.open(fileobj=archive,mode='w:gz') as bundle:
                for name, data in [('bin/owlet-video',b'test-binary'),('web/index.html',b'test-page')]:
                    info=tarfile.TarInfo(name); info.size=len(data); bundle.addfile(info,io.BytesIO(data))
            archive.seek(0)
            targets=[]
            def restarted(): targets.append(current.resolve().name)
            with patch.multiple(deploy,BASE=base,RELEASES=releases,CURRENT=current), patch.dict(os.environ,{'SSH_ORIGINAL_COMMAND':'deploy '+'b'*40}), patch.object(deploy.sys,'stdin',SimpleNamespace(buffer=archive)), patch.object(deploy,'restart',side_effect=restarted), patch.object(deploy,'healthy',side_effect=[False,True]) as checked:
                with self.assertRaisesRegex(RuntimeError,'health check failed'): deploy.main()
                self.assertEqual(current.resolve(),old)
                self.assertEqual(targets,['b'*40,'a'*40])
                self.assertEqual(checked.call_count,2)


if __name__ == '__main__': unittest.main()
