# Shell integration for interactive-terminal-mcp.
#
# It tells the terminal where each command begins and ends, and how it ended,
# using the OSC 133 sequences that iTerm2, WezTerm, Windows Terminal and VS
# Code all speak. Without it the tools can only watch output go quiet, which a
# command that prints nothing does the instant it starts.
#
# This file is passed to bash with --init-file, which replaces bash's own
# startup files. The user's must therefore be sourced here, or their shell
# would arrive with none of their configuration.

if [ -r /etc/bash.bashrc ]; then
	. /etc/bash.bashrc
fi
if [ -r "$HOME/.bashrc" ]; then
	. "$HOME/.bashrc"
fi

# Everything below is additive and deliberately leaves PS1 alone. Prompts are
# the part of a shell people customise most, and a wrapper around PS1 is both
# easy to break and easily undone by any theme that rebuilds PS1 on every
# prompt. PROMPT_COMMAND runs immediately before the prompt is drawn, which is
# the same moment, and needs no cooperation from whatever built it.
__itm_report() {
	# Captured first: anything else here would overwrite the status being
	# reported, which is the whole point of the sequence.
	local __itm_status=$?
	printf '\033]133;D;%s\007\033]133;A\007' "$__itm_status"
	return $__itm_status
}

# PS0 is printed after a command line is read and before it runs, which is
# exactly the boundary the terminal needs to know a command has started. It
# arrived in bash 4.4; older shells simply report completions without it, and
# the tools fall back to their own checks for whether something is running.
if [ "${BASH_VERSINFO[0]}" -gt 4 ] || { [ "${BASH_VERSINFO[0]}" -eq 4 ] && [ "${BASH_VERSINFO[1]}" -ge 4 ]; }; then
	PS0='\e]133;C\a'"${PS0-}"
fi

# PROMPT_COMMAND is a string in every bash before 5.1 and may be an array
# after. Appending to the wrong shape would either be ignored or corrupt the
# user's, so the declared type decides which is used.
__itm_declared=$(declare -p PROMPT_COMMAND 2>/dev/null)
case "$__itm_declared" in
	"declare -a"*)
		PROMPT_COMMAND=(__itm_report "${PROMPT_COMMAND[@]}")
		;;
	*)
		PROMPT_COMMAND="__itm_report${PROMPT_COMMAND:+;$PROMPT_COMMAND}"
		;;
esac
unset __itm_declared
