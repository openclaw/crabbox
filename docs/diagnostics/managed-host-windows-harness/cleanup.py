import json,shutil
import driver as d
import qualify as q
s=d.load()
q.E=json.loads((d.WORK/'qualification.json').read_text())
assert q.available(), 'settle the task lease through crabbox stop/restart before removing the harness'
assert d.state().get('runtime') is None, 'agent still retains a runtime'
code,_=d.api('/v1/ssh-hosts/'+s['hostID'],{'enabled':False,'draining':True},admin=True,method='PATCH');assert code==200
code,_=d.api('/v1/ssh-hosts/'+s['hostID'],admin=True,method='DELETE');assert code==200
g=s['guest']
assert g.endswith('\\'+s['prefix']) and s['prefix'].startswith('cbx-win-proof-')
result=d.remote("$root='"+g+"'; $item=Get-Item -LiteralPath $root; if($item.Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'task root is a reparse point'}; Get-CimInstance Win32_Process | Where-Object {$_.ExecutablePath -eq ($root+'\\crabbox.exe')} | ForEach-Object {Stop-Process -Id $_.ProcessId -Force}; Stop-ScheduledTask -TaskName '"+s['prefix']+"'; Unregister-ScheduledTask -TaskName '"+s['prefix']+"' -Confirm:$false; Remove-Item -LiteralPath $root -Recurse -Force; if(Test-Path -LiteralPath $root){throw 'task root remains'}; 'TASK_ROOT_ABSENT'")
assert result=='TASK_ROOT_ABSENT',result
d.run(d.SSH+['-S',str(d.WORK/'tunnel.sock'),'-O','exit',d.HOST])
for suffix in ['-coordinator','-db']:d.run(['docker','rm','-f',s['prefix']+suffix])
d.run(['docker','network','rm',s['prefix']])
q.check('Proof task, guest root, tunnel, coordinator/database containers and network removed; existing host access preserved')
q.E['status']='passed';q.save()
for name in ['agent-config.json','coord.env','state.json','wrong-key','wrong-key.pub','expiry-key','expiry-key.pub']:
 (d.WORK/name).unlink(missing_ok=True)
for name in ['home','config','cache']:
 path=d.WORK/name
 assert not path.is_symlink(), 'proof directory is a symlink'
 if path.exists():shutil.rmtree(path)
