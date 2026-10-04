package core

import (
	"context"
	"testing"
)

// smsClientStub opts a plain client into the SMS capability, so the capability
// report and the narrowing helper can be pinned without a vendor.
type smsClientStub struct{ plainClient }

func (c *smsClientStub) SMSStatus(context.Context, SMSOpts) SMSStatus { return SMSStatus{} }
func (c *smsClientStub) AcquirePhone(context.Context, SMSOpts, string, []string) (SMSNumber, error) {
	return SMSNumber{}, nil
}
func (c *smsClientStub) PollSMSCode(context.Context, SMSOpts, string) (SMSCode, error) {
	return SMSCode{}, nil
}
func (c *smsClientStub) ReleasePhone(context.Context, SMSOpts, string, bool) error { return nil }

// TestCapabilitiesOfReportsTheSMSOptIn keeps the flag and the interface in
// step: a module that implements SMSProvider must report sms=true, and one that
// does not must not.  A drift here is what would make the panel render 接码
// controls for a module that answers 501.
func TestCapabilitiesOfReportsTheSMSOptIn(t *testing.T) {
	ctx := context.Background()
	if caps := CapabilitiesOf(ctx, &smsClientStub{plainClient{name: "sms"}}); !caps.SMS {
		t.Errorf("SMS = false for a SMSProvider, want true")
	}
	if caps := CapabilitiesOf(ctx, &plainClient{name: "plain"}); caps.SMS {
		t.Errorf("SMS = true for a client with no SMSProvider, want false")
	}
}

// TestAsSMSProviderFollowsTheCapabilityConvention is the panel's 501 guard.
func TestAsSMSProviderFollowsTheCapabilityConvention(t *testing.T) {
	if _, ok := AsSMSProvider(&plainClient{name: "plain"}); ok {
		t.Errorf("AsSMSProvider accepted a module with no SMS support")
	}
	if _, ok := AsSMSProvider(&smsClientStub{plainClient{name: "sms"}}); !ok {
		t.Errorf("AsSMSProvider refused a module that implements SMSProvider")
	}
}
