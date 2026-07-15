package protocol

import "testing"

func TestCommandName(t *testing.T) {
	cases := map[byte]string{
		CmdStatusQuery:      "status_query",
		CmdRetract:          "retract",
		CmdChangeFrequency:  "change_frequency",
		CmdMovingStatus:     "moving_status",
		CmdModifyElementLen: "modify_element_len",
		0x7E:                "cmd_0x7E",
	}
	for com, want := range cases {
		if got := CommandName(com); got != want {
			t.Errorf("CommandName(%d) = %q, want %q", com, got, want)
		}
	}
}

func TestReplyName(t *testing.T) {
	cases := map[byte]string{
		ReplyOK:             "ok",
		ReplyError:          "error",
		ReplyBadParams:      "bad_params",
		ReplyInvalidCommand: "invalid_command",
		ReplyDebug:          "debug",
		0x99:                "reply_0x99",
	}
	for com, want := range cases {
		if got := ReplyName(com); got != want {
			t.Errorf("ReplyName(%d) = %q, want %q", com, got, want)
		}
	}
}
