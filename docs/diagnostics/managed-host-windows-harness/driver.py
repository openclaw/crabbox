# Proof snapshot: use only a designated Windows proof target; see README.md.
import base64,json,os,secrets,subprocess,time,urllib.request,urllib.error
from pathlib import Path
ROOT=Path(os.environ['CRABBOX_PROOF_REPO']).resolve()
WORK=Path(os.environ['CRABBOX_PROOF_EVIDENCE']).resolve()
WORK.mkdir(mode=0o700,parents=True,exist_ok=True)
SSH=['ssh','-F',os.environ['CRABBOX_PROOF_SSH_CONFIG'],'-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o','ConnectTimeout=15']
HOST=os.environ['CRABBOX_PROOF_SSH_HOST']
def run(args,**kw):
 r=subprocess.run([str(x) for x in args],text=True,capture_output=True,timeout=kw.pop('timeout',120),**kw)
 if r.returncode: raise RuntimeError(f'command exit {r.returncode}: {r.stderr[-3000:]} {r.stdout[-3000:]}')
 return r.stdout.strip()
def remote(s):
 code="$ProgressPreference='SilentlyContinue'; $ErrorActionPreference='Stop'; "+s
 return run(SSH+[HOST,'powershell.exe -NoLogo -NoProfile -EncodedCommand '+base64.b64encode(code.encode('utf-16le')).decode()])
def upload(src,name):
 remote_path=load()['guest'].replace('\\','/')+'/'+name
 return run(['scp']+SSH[1:]+[str(src),HOST+':'+remote_path])
def load(): return json.loads((WORK/'state.json').read_text())
def save(x):
 (WORK/'state.json').write_text(json.dumps(x));(WORK/'state.json').chmod(0o600)
def api(path,body=None,admin=False,owner=None,method=None):
 s=load();h={'Authorization':'Bearer '+s['admin' if admin else 'borrower'],'Content-Type':'application/json'}
 if admin and owner is None:owner='operator@example.com'
 if owner:h['X-Crabbox-Owner']=owner
 try:
  with urllib.request.urlopen(urllib.request.Request(s['url']+path,data=None if body is None else json.dumps(body).encode(),headers=h,method=method),timeout=60) as r:return r.status,json.load(r)
 except urllib.error.HTTPError as e:return e.code,json.load(e)
def env(admin=False):
 s=load();e={k:v for k,v in os.environ.items() if k in ('PATH','TMPDIR')};e.update(HOME=str(WORK/'home'),XDG_CONFIG_HOME=str(WORK/'config'),XDG_CACHE_HOME=str(WORK/'cache'),CRABBOX_CONFIG=str(WORK/'empty.yaml'),CRABBOX_COORDINATOR=s['url'],CRABBOX_COORDINATOR_TOKEN=s['admin' if admin else 'borrower'],CRABBOX_OWNER='operator@example.com' if admin else 'borrower@example.com');return e
def cli(args,**kw):return run([ROOT/'bin/crabbox']+args,env=env(),cwd=WORK/'repo',**kw)
def start_task():
 s=load();return remote("Start-ScheduledTask -TaskName '"+s['prefix']+"'; 'STARTED'")
def state():
 s=load();return json.loads(remote("Get-Content -Raw '"+s['guest']+"\\state\\state.json'"))
def summary():
 status,body=api('/v1/ssh-hosts');print(status,[(h['state'],h.get('online')) for h in body.get('hosts',[])])
 print(remote("Get-Content -Tail 12 '"+load()['guest']+"\\agent.err' -ErrorAction SilentlyContinue"))
if __name__=='__main__':
 import sys
 if sys.argv[1]=='setup':
  assert not (WORK/'state.json').exists()
  p='cbx-win-proof-'+secrets.token_hex(4);s={'prefix':p,'admin':secrets.token_hex(32),'borrower':secrets.token_hex(32),'guest':remote("[Environment]::GetFolderPath('UserProfile')")+'\\'+p,'remotePort':48763};save(s)
  (WORK/'coord.env').write_text('CRABBOX_ADMIN_TOKEN='+s['admin']+'\nCRABBOX_SHARED_TOKEN='+s['borrower']+'\nCRABBOX_SHARED_OWNER=borrower@example.com\nCRABBOX_DEFAULT_ORG=example-org\nDATABASE_URL=postgresql://postgres@db/postgres\nPORT=8080\nCRABBOX_PORTABLE_POOLS_ENABLED=true\n');(WORK/'coord.env').chmod(0o600)
  run(['docker','network','create',p]);run(['docker','run','-d','--name',p+'-db','--network',p,'--network-alias','db','-e','POSTGRES_HOST_AUTH_METHOD=trust','postgres:17-bookworm'])
  time.sleep(2)
  run(['docker','run','-d','--name',p+'-coordinator','--network',p,'--env-file',WORK/'coord.env','-p','127.0.0.1::8080','-v',str(ROOT/'worker')+':/worker:ro','-w','/worker','node:24.22.1-bookworm','node','dist-node/server.mjs'])
  port=run(['docker','port',p+'-coordinator','8080/tcp']).split(':')[-1];s['url']='http://127.0.0.1:'+port;save(s)
  run(SSH+['-f','-N','-o','ExitOnForwardFailure=yes','-o','ControlMaster=yes','-o','ControlPath='+str(WORK/'tunnel.sock'),'-R',f"127.0.0.1:{s['remotePort']}:127.0.0.1:{port}",HOST])
  print(remote("$p='"+s['guest']+"'; New-Item -ItemType Directory -Path $p | Out-Null; $u=[Security.Principal.WindowsIdentity]::GetCurrent().Name; & icacls $p /inheritance:r /grant:r ($u+':(OI)(CI)F') '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null; if($LASTEXITCODE){throw 'ACL'}; 'PRIVATE_ROOT_READY'"))
  for src,name in [(ROOT/'bin/crabbox-host-windows.exe','crabbox.exe'),(Path(os.environ['CRABBOX_PROOF_MXC']),'wxc-exec.exe'),(Path(os.environ['CRABBOX_PROOF_MXC']).with_name('plm.exe'),'plm.exe')]:upload(src,name)
  (WORK/'agent-config.json').write_text(json.dumps({'url':'http://127.0.0.1:'+str(s['remotePort']),'token':s['admin']}));(WORK/'agent-config.json').chmod(0o600);upload(WORK/'agent-config.json','config.json')
  script=r'''$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'
Set-Location $PSScriptRoot
$c=Get-Content -Raw "$PSScriptRoot\config.json"|ConvertFrom-Json
$env:CRABBOX_COORDINATOR=$c.url;$env:CRABBOX_COORDINATOR_TOKEN=$c.token;$env:CRABBOX_OWNER='operator@example.com';$env:CRABBOX_CONFIG="$PSScriptRoot\empty.yaml"
[IO.File]::WriteAllText($env:CRABBOX_CONFIG,"{}")
[IO.File]::WriteAllText("$PSScriptRoot\launcher.json",(@{pid=$PID;identity=[Security.Principal.WindowsIdentity]::GetCurrent().Name;session=[Diagnostics.Process]::GetCurrentProcess().SessionId}|ConvertTo-Json))
if(-not (Test-Path "$PSScriptRoot\state\state.json")) { & .\crabbox.exe host enroll --state-dir "$PSScriptRoot\state" --name windows-proof --template proof-v1 --capabilities test 1> "$PSScriptRoot\enroll.json" 2> "$PSScriptRoot\enroll.err"; if($LASTEXITCODE){exit $LASTEXITCODE} }
$p=Start-Process -FilePath "$PSScriptRoot\crabbox.exe" -ArgumentList @('host','serve','--state-dir',"$PSScriptRoot\state",'--mxc-executable',"$PSScriptRoot\wxc-exec.exe") -PassThru -RedirectStandardOutput "$PSScriptRoot\agent.out" -RedirectStandardError "$PSScriptRoot\agent.err" -NoNewWindow
$p.Id | Set-Content "$PSScriptRoot\agent.pid"
$p.WaitForExit(); $p.Refresh(); $p.ExitCode | Set-Content "$PSScriptRoot\agent.exit"
'''
  (WORK/'agent.ps1').write_text(script);upload(WORK/'agent.ps1','agent.ps1')
  print(remote("$a=New-ScheduledTaskAction -Execute 'powershell.exe' -Argument '-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "+s['guest']+"\\agent.ps1'; $p=New-ScheduledTaskPrincipal -UserId ([Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive -RunLevel Limited; $settings=New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Hours 2); Register-ScheduledTask -TaskName '"+p+"' -Action $a -Principal $p -Settings $settings | Out-Null; 'TASK_READY'"))
  for d in ['home','config','cache']:(WORK/d).mkdir(exist_ok=True)
  (WORK/'home/Library/Caches').mkdir(parents=True,exist_ok=True)
  (WORK/'empty.yaml').write_text('{}');run(['git','clone','--quiet','--depth=1','https://github.com/githubtraining/hellogitworld.git',WORK/'repo']);(WORK/'repo/hello.txt').write_text('windows-borrower-sync-proof\n');run(['git','-C',WORK/'repo','add','hello.txt'])
  print(start_task())
  for _ in range(30):
   try:host=state()['id'];break
   except (RuntimeError,KeyError):time.sleep(1)
  else:raise RuntimeError('agent enrollment did not complete')
  s=load();s['hostID']=host;save(s)
  code,_=api('/v1/ssh-hosts/'+host,{'enabled':True},admin=True,method='PATCH')
  assert code==200,code
 elif sys.argv[1]=='summary':summary()
