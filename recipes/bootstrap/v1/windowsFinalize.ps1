
	git --version | Out-Null
	tar --version | Out-Null
	Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath -Value (Get-Date).ToString("o")
	Restart-Service sshd -Force
