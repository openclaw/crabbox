import json,subprocess,time
import driver as d
import qualify as q
q.E=json.loads((d.WORK/'qualification.json').read_text())
q.E.pop('error',None)
keys=[]
try:
 q.cli(['warmup','--provider','ssh-host','--target','windows','--arch','arm64','--ttl','30m','--idle-timeout','15m'])
 lease=q.active()
 identity=d.WORK/'pool-identity.json'
 identity.write_text(q.cli(['pool','identity','builders','--id',lease['id'],'--cache-compatibility','proof-v1'])+'\n')
 q.cli(['pool','register','builders','--id',lease['id'],'--identity-file',str(identity),'--compatibility-key','proof','--access'])
 for generation in range(2):
  previous=d.state().get('assignment',{}).get('accessGeneration',0)
  args=['run','--provider','ssh-host','--target','windows','--pool','builders','--pool-access','--pool-identity-file',str(identity),'--pool-compatibility-key','proof','--pool-duration','5m','--no-hydrate','--shell',"$p=Join-Path $env:USERPROFILE 'previous-borrower'; if(Test-Path $p){throw 'stale borrower'}; Set-Content $p 'owned'; Get-Content hello.txt; Write-Output 'pooled-windows-ok'; Start-Sleep -Seconds 3"]
  with (d.WORK/f'pool-{generation}.out').open('w') as out,(d.WORK/f'pool-{generation}.err').open('w') as err:
   p=subprocess.Popen([str(d.ROOT/'bin/crabbox')]+args,cwd=d.WORK/'repo',env=d.env(),stdout=out,stderr=err)
  def preparing():
   if p.poll() is not None and p.returncode:
    raise AssertionError((d.WORK/f'pool-{generation}.err').read_text()[-3000:])
   state=d.state()
   return bool(state.get('runtime')) and state.get('assignment',{}).get('accessGeneration',0)>previous and state.get('report',{}).get('state')=='ready'
  q.eventually(preparing,120)
  r,a=q.capture_runtime();keys.append(r['hostKey'])
  assert p.wait(timeout=300)==0,(d.WORK/f'pool-{generation}.err').read_text()[-3000:]
  output=(d.WORK/f'pool-{generation}.out').read_text()
  assert 'pooled-windows-ok' in output and 'windows-borrower-sync-proof' in output,output
  q.cleanup(r)
 assert keys[0]!=keys[1]
 q.check('Windows portable pool identity, registration, two synced borrower generations and cleanup with rotated endpoint keys',generations=2)
 q.cli(['stop','--provider','ssh-host','--target','windows','--id',lease['id']],timeout=120);q.eventually(q.available)
 # Direct API admission shortens only the fixture lease; the real endpoint expires it.
 key=d.WORK/'expiry-key'
 if not key.exists():d.run(['ssh-keygen','-q','-t','ed25519','-N','','-f',key])
 body={'provider':'ssh-host','leaseID':'cbx_00000000ee01','sshPublicKey':key.with_suffix('.pub').read_text().strip(),'target':'windows','architecture':'arm64','ttlSeconds':90,'idleTimeoutSeconds':90}
 code,result=d.api('/v1/leases',body,admin=True,owner='expiry@example.com');assert code==202,(code,result)
 q.eventually(lambda:d.state().get('report',{}).get('state')=='ready',90)
 r,a=q.capture_runtime()
 q.eventually(q.available,150);q.cleanup(r)
 expired=next(l for l in d.api('/v1/leases',admin=True)[1]['leases'] if l['id']==body['leaseID'])
 assert expired['state']=='expired',expired['state']
 q.check('Unattended Windows lease expiry terminates exact session and removes account/workspace before reuse')
 q.E['status']='passed';q.save()
except Exception as e:
 q.E['status']='failed';q.E['error']=str(e);q.save();raise
