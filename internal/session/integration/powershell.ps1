# Shell integration for interactive-terminal-mcp.
#
# It reports how each command ended, using the OSC 133 sequences Windows
# Terminal, VS Code and others speak. Without it the only completion signal on
# Windows is whether the shell has spawned a child process, which says nothing
# about the cmdlets PowerShell runs inside itself.
#
# Only the prompt boundary and the exit status are reported. PowerShell has no
# hook that fires between reading a command line and running it, so there is no
# honest place to mark the start of execution; the terminal is told nothing
# about that and keeps using its own check.

# The profile has already run, so the prompt is whatever the user built. It is
# captured and called, never replaced.
#
# $Function:prompt is the script block itself. Get-Item Function:\prompt looks
# the right thing up but hands back a reference by *name*, so calling it after
# the redefinition below calls the new prompt instead of the old one -- which
# means it calls itself. That recursed until PowerShell stopped it, returning a
# prompt of twenty thousand repeated marks and no completion mark at all.
if (Test-Path Function:\prompt) {
	$Global:__ITMOriginalPrompt = $Function:prompt
}
$Global:__ITMLastHistoryId = -1
$Global:__ITMInPrompt = $false

function Global:__ITMExitStatus {
	param($Succeeded, $LastExit)
	# $? covers cmdlets and native commands alike. $LASTEXITCODE only covers
	# native ones and keeps its value long after the command that set it, so it
	# is consulted only once $? has already said something failed.
	if ($Succeeded) { return 0 }
	if ($null -ne $LastExit -and $LastExit -ne 0) { return $LastExit }
	return 1
}

function Global:prompt {
	# Captured on the first line: everything below changes both of them.
	$succeeded = $?
	$lastExit = $LASTEXITCODE

	# Belt and braces against the failure above ever returning in another form:
	# whatever the captured prompt turns out to be, it can only be entered once.
	if ($Global:__ITMInPrompt) { return '' }
	$Global:__ITMInPrompt = $true

	$out = ''
	try {
		$status = __ITMExitStatus $succeeded $lastExit
		$entry = Get-History -Count 1

		if ($Global:__ITMLastHistoryId -ne -1) {
			if ($null -ne $entry -and $entry.Id -ne $Global:__ITMLastHistoryId) {
				$out += "$([char]27)]133;D;$status$([char]7)"
			} else {
				# Nothing new ran: an empty line, or a command interrupted before
				# it was recorded. Reporting a status here would invent one.
				$out += "$([char]27)]133;D$([char]7)"
			}
		}
		$out += "$([char]27)]133;A$([char]7)"
		if ($null -ne $entry) { $Global:__ITMLastHistoryId = $entry.Id }
	} catch {
		# A prompt that throws costs the user their prompt. Reporting nothing is
		# always better than that, so every failure here is swallowed and the
		# original prompt is still returned below.
		$out = ''
	}

	try {
		if ($null -ne $Global:__ITMOriginalPrompt) {
			$out += & $Global:__ITMOriginalPrompt
		} else {
			$out += "PS $($executionContext.SessionState.Path.CurrentLocation)$('>' * ($nestedPromptLevel + 1)) "
		}
	} finally {
		$Global:__ITMInPrompt = $false
	}
	return $out
}
