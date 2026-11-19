package trakt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DeviceCode is what the user needs to approve a device login.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// Login runs Trakt's device flow: it gets a code, hands it to show, polls
// until the user approves it, and stores the token pair.
func (c *Client) Login(ctx context.Context, show func(DeviceCode)) error {
	dc, err := c.deviceCode(ctx)
	if err != nil {
		return err
	}
	show(dc)
	tok, err := c.pollDeviceToken(ctx, dc)
	if err != nil {
		return err
	}
	if err := c.cfg.Store.SaveToken(ctx, tok); err != nil {
		return fmt.Errorf("trakt: storing the token pair: %w", err)
	}
	return nil
}

func (c *Client) deviceCode(ctx context.Context) (DeviceCode, error) {
	u := c.cfg.AuthURL + "/oauth/device/code"
	resp, err := c.send(ctx, func() (*http.Request, error) {
		return c.newRequest(ctx, http.MethodPost, u, map[string]string{"client_id": c.cfg.ClientID})
	})
	if err != nil {
		return DeviceCode{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return DeviceCode{}, statusError(resp)
	}
	defer resp.Body.Close()
	var dc DeviceCode
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return DeviceCode{}, fmt.Errorf("trakt: reading the device code: %w", err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return DeviceCode{}, errors.New("trakt: the device code answer has no code")
	}
	return dc, nil
}

// pollDeviceToken polls at the interval Trakt asks for until the user
// approves or denies the code, or it expires.
func (c *Client) pollDeviceToken(ctx context.Context, dc DeviceCode) (Token, error) {
	interval := time.Duration(dc.Interval) * time.Second
	deadline := c.now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	body := map[string]string{"code": dc.DeviceCode, "client_id": c.cfg.ClientID}
	if c.cfg.ClientSecret != "" {
		body["client_secret"] = c.cfg.ClientSecret
	}
	u := c.cfg.AuthURL + "/oauth/device/token"
	for {
		if err := c.sleep(ctx, interval); err != nil {
			return Token{}, err
		}
		if c.now().After(deadline) {
			return Token{}, errors.New("trakt: the code expired before it was approved; run login again")
		}
		req, err := c.newRequest(ctx, http.MethodPost, u, body)
		if err != nil {
			return Token{}, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return Token{}, fmt.Errorf("trakt: polling for the token: %w", err)
		}
		switch resp.StatusCode {
		case http.StatusOK:
			tok, err := c.decodeToken(resp.Body)
			resp.Body.Close()
			if err != nil {
				return Token{}, fmt.Errorf("trakt: reading the token: %w", err)
			}
			return tok, nil
		case http.StatusBadRequest:
			// Pending: the user hasn't approved yet.
		case http.StatusTooManyRequests:
			// Polling too fast. RFC 8628 says to add five seconds.
			interval += 5 * time.Second
		case http.StatusNotFound:
			resp.Body.Close()
			return Token{}, errors.New("trakt: Trakt doesn't know the device code")
		case http.StatusConflict:
			resp.Body.Close()
			return Token{}, errors.New("trakt: the code was already used; run login again")
		case http.StatusGone:
			resp.Body.Close()
			return Token{}, errors.New("trakt: the code expired before it was approved; run login again")
		case http.StatusTeapot:
			resp.Body.Close()
			return Token{}, errors.New("trakt: the login was denied")
		default:
			return Token{}, statusError(resp)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
