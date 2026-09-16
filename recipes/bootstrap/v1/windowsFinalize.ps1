
	git --version | Out-Null
	tar --version | Out-Null
	Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath -Value (Get-Date).ToString("o")
	# Keep this last: Node and the other baselines update machine PATH above.
	Restart-Service sshd -Force
