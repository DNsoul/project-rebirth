# Makes the task that starts Rebirth when someone signs in to Windows.
#
# Run by the installer (installer\rebirth.iss) when "start with Windows" is
# chosen. ASCII only, on purpose: Windows PowerShell 5.1 reads a script without a
# byte order mark in the system's code page.
#
# Why a task and not the Run key or the Startup folder: the program needs
# administrator rights, and a task can be set to start with the highest rights its
# user has, without the question Windows would otherwise ask at every sign-in. It
# is for any member of the Users group, so it works for whoever signs in.
#
# It starts the program with --autostart: hidden in the notification area, and
# connecting by itself.
param(
    [Parameter(Mandatory = $true)][string]$Exe
)

$ErrorActionPreference = 'Stop'

$action = New-ScheduledTaskAction -Execute $Exe -Argument '--autostart' `
    -WorkingDirectory (Split-Path -Parent $Exe)

# A little after sign-in, so that the network and the notification area are up.
$trigger = New-ScheduledTaskTrigger -AtLogOn
$trigger.Delay = 'PT15S'

$principal = New-ScheduledTaskPrincipal -GroupId 'S-1-5-32-545' -RunLevel Highest

# A VPN is wanted on a laptop's battery as much as anywhere, and it is not a job
# that has an end.
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -StartWhenAvailable -MultipleInstances IgnoreNew

Register-ScheduledTask -TaskName 'Rebirth' `
    -Action $action -Trigger $trigger -Principal $principal -Settings $settings `
    -Description 'Starts Rebirth when a user signs in.' -Force | Out-Null
