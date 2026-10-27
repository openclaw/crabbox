import base64,json,socket,subprocess,time
import driver as d
W=d.WORK
E={'scope':'Native Windows ARM64 build 28000; pinned MXC 0.8.0; isolated loopback Node/PostgreSQL coordinator; synthetic identities','checks':[],'authority':{}}
def save(): (W/'qualification.json').write_text(json.dumps(E,indent=2)+'\n')
def check(name,**detail):
 E['checks'].append({'name':name,'status':'passed',**detail});save();print('PASS',name,flush=True)
def eventually(fn,seconds=120):
 end=time.monotonic()+seconds
 while time.monotonic()<end:
  value=fn()
  if value:return value
  time.sleep(1)
 raise AssertionError('condition did not settle')
def available():return d.api('/v1/ssh-hosts')[1]['hosts'][0]['state']=='available'
def active():return next(l for l in d.api('/v1/leases')[1]['leases'] if l.get('provider')=='ssh-host' and l['state']=='active')
def cleanup(r):
 user=r['home'].split('\\')[2]
 v=json.loads(d.remote("@{homeExists=(Test-Path -LiteralPath '"+r['home']+"');endpointCount=@(Get-CimInstance Win32_Process | Where-Object {$_.ExecutablePath -eq '"+r['home']+"\\crabbox-endpoint.exe'}).Count; accountExists=([bool](Get-CimInstance Win32_UserAccount | Where-Object {$_.Name -eq '"+user+"'}));sessionProcessCount=@(Get-CimInstance Win32_Process | Where-Object {$_.SessionId -eq "+str(r.get('sessionID',-999))+"}).Count} | ConvertTo-Json -Compress"))
 assert not v['homeExists'] and not v['accountExists'] and v['endpointCount']==0 and v['sessionProcessCount']==0,v
 return v
def cli(args,admin=False,timeout=300):
 return d.run([d.ROOT/'bin/crabbox']+args,env=d.env(admin),cwd=W/'repo',timeout=timeout)
def capture_runtime():
 s=d.state();r=s['runtime'];a=s['assignment']
 p=json.loads(d.remote("Get-CimInstance Win32_Process | Where-Object {$_.ExecutablePath -eq '"+r['home']+"\\crabbox-endpoint.exe'} | Select-Object ProcessId,SessionId | ConvertTo-Json -Compress"));r['sessionID']=p['SessionId'];return r,a
if __name__ == "__main__":
 try:
  if not (W/'borrow-fixed.log').exists():
   result=cli(['run','--provider','ssh-host','--target','windows','--arch','arm64','--keep','--ttl','30m','--idle-timeout','15m','--shell',"Get-Content hello.txt; Write-Output 'windows-managed-exec-ok'; whoami"])
   (W/'borrow-fixed.log').write_text(result)
  assert 'windows-managed-exec-ok' in (W/'borrow-fixed.log').read_text()
  lease=active();r,a=capture_runtime();key1=lease['sshHostKey']
  operator=json.loads(d.remote("Get-Content -Raw '"+d.load()['guest']+"\\launcher.json'"))['session']
  assert r['sessionID']!=operator
  check('Shipped CLI sync and allowed execution in an MXC borrower session',operatorSession=operator,borrowerSession=r['sessionID'])
  code,_=d.api('/v1/serve/leases/'+lease['id']+'/relay',{},admin=True,owner='foreign@example.com');assert code==403,code
  E['authority']['foreignIdentityRelayHTTP']=code
  key=W/'wrong-key'
  if not key.exists():d.run(['ssh-keygen','-q','-t','ed25519','-N','','-f',key])
  with socket.socket() as s:s.bind(('127.0.0.1',0));local=s.getsockname()[1]
  forward=f'127.0.0.1:{local}:127.0.0.1:{r["port"]}'
  d.run(d.SSH+['-S',str(W/'tunnel.sock'),'-O','forward','-L',forward,d.HOST])
  known=W/'endpoint-known-hosts';known.write_text('windows-proof '+lease['sshHostKey']+'\n')
  forbidden=subprocess.run(['ssh','-o','BatchMode=yes','-o','IdentitiesOnly=yes','-o','StrictHostKeyChecking=yes','-o','HostKeyAlias=windows-proof','-o','UserKnownHostsFile='+str(known),'-o','ConnectTimeout=5','-i',str(key),'-p',str(local),'crabbox@127.0.0.1','echo forbidden>foreign-executed.txt'],capture_output=True,text=True,timeout=15)
  d.run(d.SSH+['-S',str(W/'tunnel.sock'),'-O','cancel','-L',forward,d.HOST])
  assert forbidden.returncode==255 and 'Permission denied' in forbidden.stderr,forbidden.stderr
  assert d.remote("Test-Path -LiteralPath '"+r['home']+"\\work\\foreign-executed.txt'")=='False'
  E['authority']['wrongEndpointKey']={'exit':255,'effectFileExists':False};check('Foreign relay identity and wrong endpoint key denied without final execution')
  out=W/'retained.out';err=W/'retained.err'
  with out.open('w') as o,err.open('w') as e:
   retained=subprocess.Popen([str(d.ROOT/'bin/crabbox'),'connect','--provider','ssh-host','--target','windows','--id',lease['id']],env=d.env(),cwd=W/'repo',stdin=subprocess.PIPE,stdout=o,stderr=e,text=True)
  retained.stdin.write('@echo off\r\necho retained-ready\r\n');retained.stdin.flush()
  eventually(lambda:'\nretained-ready\n' in out.read_text(),60)
  path=r['home']+'\\work\\child-heartbeat.txt'
  body="while($true){[IO.File]::AppendAllText('"+path+"', 'tick'+[Environment]::NewLine);Start-Sleep -Milliseconds 200}"
  encoded=base64.b64encode(body.encode('utf-16le')).decode()
  command="$p=Start-Process powershell.exe -ArgumentList @('-NoLogo','-NoProfile','-NonInteractive','-EncodedCommand','"+encoded+"') -PassThru; Write-Output ('CHILD_PID='+$p.Id)"
  childout=cli(['run','--provider','ssh-host','--target','windows','--id',lease['id'],'--no-sync','--keep','--shell',command]);assert 'CHILD_PID=' in childout,childout
  eventually(lambda:d.remote("Test-Path -LiteralPath '"+path+"'")=='True',30)
  launch=d.remote("$n=0; Get-CimInstance Win32_Process | Where-Object {$_.ExecutablePath -eq '"+d.load()['guest']+"\\wxc-exec.exe'} | ForEach-Object {if($_.CommandLine -match '--config-base64 ([A-Za-z0-9+/=]+)'){ $c=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($Matches[1]))|ConvertFrom-Json; if($c.phase -eq 'exec' -and $c.sandboxId -eq '"+r['sandboxID']+"'){Stop-Process -Id $_.ProcessId -Force;$n++}}}; $n")
  retained.stdin.write('echo launcher-independent\r\n');retained.stdin.flush();eventually(lambda:'\nlauncher-independent\n' in out.read_text(),30)
  check('Borrower authority persists after exact MXC exec launcher exits',launchersStopped=int(launch))
  cli(['stop','--provider','ssh-host','--target','windows','--id',lease['id']],timeout=120)
  eventually(available)
  assert retained.wait(timeout=30)!=0
  try:retained.stdin.write('echo forbidden-after-revocation>revoked.txt\r\n');retained.stdin.flush()
  except (BrokenPipeError,OSError):pass
  try:retained.stdin.close()
  except (BrokenPipeError,OSError):pass
  assert 'forbidden-after-revocation' not in out.read_text()
  code,_=d.api('/v1/serve/leases/'+lease['id']+'/relay',{});assert code==403,code
  receipt=cleanup(r);E['authority']['revokedNewRelayHTTP']=code;E['authority']['retainedConnection']='ready and executable after MXC launcher exit; disconnected after revoke; no post-revocation output';check('Revocation terminates retained shell and detached process, removes exact account/workspace before reuse',**receipt)
  foreignout=cli(['run','--provider','ssh-host','--target','windows','--arch','arm64','--keep','--ttl','20m','--idle-timeout','10m','--shell',"if(Test-Path child-heartbeat.txt){throw 'stale borrower file'};Write-Output 'next-owner-ok'"],admin=True)
  assert 'next-owner-ok' in foreignout
  foreign=next(l for l in d.api('/v1/leases',admin=True)[1]['leases'] if l['state']=='active');r2,a2=capture_runtime()
  assert foreign['sshHostKey']!=key1 and r2['home']!=r['home']
  code,_=d.api('/v1/serve/leases/'+foreign['id']+'/relay',{});assert code==403,code
  E['authority']['foreignNonAdminRelayHTTP']=code;check('New owner gets fresh session and rotated endpoint key; authenticated non-admin foreign borrower denied')
  cli(['stop','--provider','ssh-host','--target','windows','--id',foreign['id']],admin=True,timeout=120);eventually(available);cleanup(r2)
  cli(['run','--provider','ssh-host','--target','windows','--arch','arm64','--keep','--ttl','15m','--idle-timeout','5m','--shell',"Write-Output 'restart-before-ok'"])
  interrupted=active();r3,a3=capture_runtime()
  print(d.remote("$p=Get-CimInstance Win32_Process | Where-Object {$_.ExecutablePath -eq '"+d.load()['guest']+"\\crabbox.exe'}; if(@($p).Count -ne 1){throw 'agent identity'}; $p | ForEach-Object {Stop-Process -Id $_.ProcessId -Force}; 'AGENT_CRASHED'"),flush=True)
  time.sleep(2);d.start_task();eventually(available,120);cleanup(r3)
  ended=next(l for l in d.api('/v1/leases')[1]['leases'] if l['id']==interrupted['id']);assert ended['state']=='failed',ended['state']
  check('Agent crash/restart cleans persistent MXC session and fails interrupted lease before reuse')
  E['status']='passed';save()
 except Exception as e:
  E['status']='failed';E['error']=str(e);save();raise
