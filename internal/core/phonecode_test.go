package core

import (
	"context"
	"testing"
)

type phoneCodeClientStub struct{ plainClient }

func (c *phoneCodeClientStub) StartPhoneCodeLogin(context.Context, string) (PhoneCodeChallenge, error) {
	return PhoneCodeChallenge{ID: "challenge-1"}, nil
}

func (c *phoneCodeClientStub) FinishPhoneCodeLogin(context.Context, string, string) (AccountRecord, error) {
	return AccountRecord{ID: "account-1"}, nil
}

func TestCapabilitiesOfReportsThePhoneCodeOptIn(t *testing.T) {
	ctx := context.Background()
	if caps := CapabilitiesOf(ctx, &phoneCodeClientStub{plainClient{name: "phone"}}); !caps.PhoneCode {
		t.Fatal("PhoneCode = false for a PhoneCodeLoginProvider, want true")
	}
	if caps := CapabilitiesOf(ctx, &plainClient{name: "plain"}); caps.PhoneCode {
		t.Fatal("PhoneCode = true for a client with no PhoneCodeLoginProvider, want false")
	}
}

func TestAsPhoneCodeLoginProviderFollowsTheCapabilityConvention(t *testing.T) {
	if _, ok := AsPhoneCodeLoginProvider(&plainClient{name: "plain"}); ok {
		t.Fatal("AsPhoneCodeLoginProvider accepted a module with no phone-code support")
	}
	if _, ok := AsPhoneCodeLoginProvider(&phoneCodeClientStub{plainClient{name: "phone"}}); !ok {
		t.Fatal("AsPhoneCodeLoginProvider refused a module that implements PhoneCodeLoginProvider")
	}
}
