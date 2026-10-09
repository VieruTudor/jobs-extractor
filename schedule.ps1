param(
    [string]$Time = '09:00',
    [switch]$Remove
)
$ErrorActionPreference = 'Stop'
$taskName = 'Jobs listing extractor - daily'
if ($Remove) {
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
    exit
}
$pythonPath = (Get-Command python -ErrorAction Stop).Source
$scriptPath = Join-Path $PSScriptRoot 'jobs.py'
$action = New-ScheduledTaskAction -Execute $pythonPath -Argument ('"' + $scriptPath + '" collect') -WorkingDirectory $PSScriptRoot
$trigger = New-ScheduledTaskTrigger -Daily -At $Time
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -MultipleInstances IgnoreNew -ExecutionTimeLimit (New-TimeSpan -Hours 2)
$principal = New-ScheduledTaskPrincipal -UserId ([System.Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive -RunLevel Limited
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Settings $settings -Principal $principal -Description 'Collect public job feeds and refresh local exports daily.'
