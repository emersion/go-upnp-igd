// Copyright (C) 2014 The Syncthing Authors.
//
// Adapted from https://github.com/jackpal/Taipei-Torrent/blob/dd88a8bfac6431c01d959ce3c745e74b8a911793/IGD.go
// Copyright (c) 2010 Jack Palevich (https://github.com/jackpal/Taipei-Torrent/blob/dd88a8bfac6431c01d959ce3c745e74b8a911793/LICENSE)
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
//    * Redistributions of source code must retain the above copyright
// notice, this list of conditions and the following disclaimer.
//    * Redistributions in binary form must reproduce the above
// copyright notice, this list of conditions and the following disclaimer
// in the documentation and/or other materials provided with the
// distribution.
//    * Neither the name of Google Inc. nor the names of its
// contributors may be used to endorse or promote products derived from
// this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package igd

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"
)

type nopLogger struct{}

func (nopLogger) Printf(format string, v ...interface{}) {}
func (nopLogger) Println(v ...interface{})               {}

var Logger interface {
	Printf(format string, v ...interface{})
	Println(v ...interface{})
} = nopLogger{}

type upnpService struct {
	ID         string `xml:"serviceId"`
	Type       string `xml:"serviceType"`
	ControlURL string `xml:"controlURL"`
}

type upnpDevice struct {
	DeviceType   string        `xml:"deviceType"`
	FriendlyName string        `xml:"friendlyName"`
	Devices      []upnpDevice  `xml:"deviceList>device"`
	Services     []upnpService `xml:"serviceList>service"`
}

type upnpRoot struct {
	Device upnpDevice `xml:"device"`
}

// Discover discovers UPnP InternetGatewayDevices.
// The order in which the devices appear in the results list is not deterministic.
func Discover(ch chan<- Device, timeout time.Duration) error {
	defer close(ch)

	interfaces, err := net.Interfaces()
	if err != nil {
		//l.Println("Listing network interfaces:", err)
		return err
	}

	resultChan := make(chan IGD)
	wg := &sync.WaitGroup{}

	for _, intf := range interfaces {
		// Interface flags seem to always be 0 on Windows
		if runtime.GOOS != "windows" && (intf.Flags&net.FlagUp == 0 || intf.Flags&net.FlagMulticast == 0) {
			continue
		}

		for _, deviceType := range []string{"urn:schemas-upnp-org:device:InternetGatewayDevice:1", "urn:schemas-upnp-org:device:InternetGatewayDevice:2"} {
			wg.Add(1)
			go func(intf net.Interface, deviceType string) {
				discover(&intf, deviceType, timeout, resultChan)
				wg.Done()
			}(intf, deviceType)
		}
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	seenResults := make(map[string]bool)
nextResult:
	for result := range resultChan {
		if seenResults[result.ID()] {
			Logger.Printf("Skipping duplicate result %s with services:", result.uuid)
			for _, service := range result.services {
				Logger.Printf("* [%s] %s", service.ID, service.URL)
			}
			continue nextResult
		}

		result := result // Reallocate as we need to keep a pointer
		ch <- &result
		seenResults[result.ID()] = true

		Logger.Printf("UPnP discovery result %s with services:", result.uuid)
		for _, service := range result.services {
			Logger.Printf("* [%s] %s", service.ID, service.URL)
		}
	}

	return nil
}

// Search for UPnP InternetGatewayDevices for <timeout> seconds, ignoring responses from any devices listed in knownDevices.
// The order in which the devices appear in the result list is not deterministic
func discover(intf *net.Interface, deviceType string, timeout time.Duration, results chan<- IGD) {
	ssdp := &net.UDPAddr{IP: []byte{239, 255, 255, 250}, Port: 1900}

	tpl := `M-SEARCH * HTTP/1.1
HOST: %s
ST: %s
MAN: "ssdp:discover"
MX: %d
USER-AGENT: syncthing/1.0

`
	searchStr := fmt.Sprintf(tpl, ssdp, deviceType, timeout/time.Second)

	search := []byte(strings.Replace(searchStr, "\n", "\r\n", -1))

	Logger.Println("Starting discovery of device type", deviceType, "on", intf.Name)

	socket, err := net.ListenMulticastUDP("udp4", intf, &net.UDPAddr{IP: ssdp.IP})
	if err != nil {
		Logger.Println(err)
		return
	}
	defer socket.Close() // Make sure our socket gets closed

	err = socket.SetDeadline(time.Now().Add(timeout))
	if err != nil {
		Logger.Println(err)
		return
	}

	Logger.Println("Sending search request for device type", deviceType, "on", intf.Name)

	_, err = socket.WriteTo(search, ssdp)
	if err != nil {
		if e, ok := err.(net.Error); !ok || !e.Timeout() {
			Logger.Println(err)
		}
		return
	}

	Logger.Println("Listening for UPnP response for device type", deviceType, "on", intf.Name)

	// Listen for responses until a timeout is reached
	for {
		resp := make([]byte, 65536)
		n, _, err := socket.ReadFrom(resp)
		if err != nil {
			if e, ok := err.(net.Error); !ok || !e.Timeout() {
				Logger.Println("UPnP read:", err) //legitimate error, not a timeout.
			}
			break
		}
		igd, err := parseResponse(deviceType, resp[:n])
		if err != nil {
			Logger.Println("UPnP parse:", err)
			continue
		}
		results <- igd
	}
	Logger.Println("Discovery for device type", deviceType, "on", intf.Name, "finished.")
}

func parseResponse(deviceType string, resp []byte) (IGD, error) {
	Logger.Println("Handling UPnP response:\n\n" + string(resp))

	reader := bufio.NewReader(bytes.NewBuffer(resp))
	request := &http.Request{}
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return IGD{}, err
	}

	respondingDeviceType := response.Header.Get("St")
	if respondingDeviceType != deviceType {
		return IGD{}, errors.New("unrecognized UPnP device of type " + respondingDeviceType)
	}

	deviceDescriptionLocation := response.Header.Get("Location")
	if deviceDescriptionLocation == "" {
		return IGD{}, errors.New("invalid IGD response: no location specified")
	}

	deviceDescriptionURL, err := url.Parse(deviceDescriptionLocation)

	if err != nil {
		Logger.Println("Invalid IGD location: " + err.Error())
	}

	deviceUSN := response.Header.Get("USN")
	if deviceUSN == "" {
		return IGD{}, errors.New("invalid IGD response: USN not specified")
	}

	deviceUUID := strings.TrimPrefix(strings.Split(deviceUSN, "::")[0], "uuid:")
	response, err = http.Get(deviceDescriptionLocation)
	if err != nil {
		return IGD{}, err
	}
	defer response.Body.Close()

	if response.StatusCode >= 400 {
		return IGD{}, errors.New("bad status code:" + response.Status)
	}

	var upnpRoot upnpRoot
	err = xml.NewDecoder(response.Body).Decode(&upnpRoot)
	if err != nil {
		return IGD{}, err
	}

	services, err := getServiceDescriptions(deviceDescriptionLocation, upnpRoot.Device)
	if err != nil {
		return IGD{}, err
	}

	// Figure out our IP number, on the network used to reach the IGD.
	// We do this in a fairly roundabout way by connecting to the IGD and
	// checking the address of the local end of the socket. I'm open to
	// suggestions on a better way to do this...
	localIPAddress, err := localIP(deviceDescriptionURL)
	if err != nil {
		return IGD{}, err
	}

	return IGD{
		uuid:           deviceUUID,
		friendlyName:   upnpRoot.Device.FriendlyName,
		url:            deviceDescriptionURL,
		services:       services,
		localIPAddress: localIPAddress,
	}, nil
}

func localIP(url *url.URL) (net.IP, error) {
	conn, err := net.DialTimeout("tcp", url.Host, time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	localIPAddress, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return nil, err
	}

	return net.ParseIP(localIPAddress), nil
}

func getChildDevices(d upnpDevice, deviceType string) []upnpDevice {
	var result []upnpDevice
	for _, dev := range d.Devices {
		if dev.DeviceType == deviceType {
			result = append(result, dev)
		}
	}
	return result
}

func getChildServices(d upnpDevice, serviceType string) []upnpService {
	var result []upnpService
	for _, service := range d.Services {
		if service.Type == serviceType {
			result = append(result, service)
		}
	}
	return result
}

func getServiceDescriptions(rootURL string, device upnpDevice) ([]IGDService, error) {
	var result []IGDService

	if device.DeviceType == "urn:schemas-upnp-org:device:InternetGatewayDevice:1" {
		descriptions := getIGDServices(rootURL, device,
			"urn:schemas-upnp-org:device:WANDevice:1",
			"urn:schemas-upnp-org:device:WANConnectionDevice:1",
			[]string{"urn:schemas-upnp-org:service:WANIPConnection:1", "urn:schemas-upnp-org:service:WANPPPConnection:1"})

		result = append(result, descriptions...)
	} else if device.DeviceType == "urn:schemas-upnp-org:device:InternetGatewayDevice:2" {
		descriptions := getIGDServices(rootURL, device,
			"urn:schemas-upnp-org:device:WANDevice:2",
			"urn:schemas-upnp-org:device:WANConnectionDevice:2",
			[]string{"urn:schemas-upnp-org:service:WANIPConnection:2", "urn:schemas-upnp-org:service:WANPPPConnection:2"})

		result = append(result, descriptions...)
	} else {
		return result, errors.New("[" + rootURL + "] Malformed root device description: not an InternetGatewayDevice.")
	}

	if len(result) < 1 {
		return result, errors.New("[" + rootURL + "] Malformed device description: no compatible service descriptions found.")
	}
	return result, nil
}

func getIGDServices(rootURL string, device upnpDevice, wanDeviceURN string, wanConnectionURN string, URNs []string) []IGDService {
	var result []IGDService

	devices := getChildDevices(device, wanDeviceURN)

	if len(devices) < 1 {
		Logger.Println(rootURL, "- malformed InternetGatewayDevice description: no WANDevices specified.")
		return result
	}

	for _, device := range devices {
		connections := getChildDevices(device, wanConnectionURN)

		if len(connections) < 1 {
			Logger.Println(rootURL, "- malformed ", wanDeviceURN, "description: no WANConnectionDevices specified.")
		}

		for _, connection := range connections {
			for _, URN := range URNs {
				services := getChildServices(connection, URN)

				Logger.Println(rootURL, "- no services of type", URN, " found on connection.")

				for _, service := range services {
					if len(service.ControlURL) == 0 {
						Logger.Println(rootURL+"- malformed", service.Type, "description: no control URL.")
					} else {
						u, _ := url.Parse(rootURL)
						replaceRawPath(u, service.ControlURL)

						Logger.Println(rootURL, "- found", service.Type, "with URL", u)

						service := IGDService{ID: service.ID, URL: u.String(), URN: service.Type}

						result = append(result, service)
					}
				}
			}
		}
	}

	return result
}

func replaceRawPath(u *url.URL, rp string) {
	asURL, err := url.Parse(rp)
	if err != nil {
		return
	} else if asURL.IsAbs() {
		u.Path = asURL.Path
		u.RawQuery = asURL.RawQuery
	} else {
		var p, q string
		fs := strings.Split(rp, "?")
		p = fs[0]
		if len(fs) > 1 {
			q = fs[1]
		}

		if p[0] == '/' {
			u.Path = p
		} else {
			u.Path += p
		}
		u.RawQuery = q
	}
}

func soapRequest(url, service, function, message string) ([]byte, error) {
	tpl := `<?xml version="1.0" ?>
	<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
	<s:Body>%s</s:Body>
	</s:Envelope>
`
	var resp []byte

	body := fmt.Sprintf(tpl, message)

	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return resp, err
	}
	req.Close = true
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("User-Agent", "syncthing/1.0")
	req.Header["SOAPAction"] = []string{fmt.Sprintf(`"%s#%s"`, service, function)} // Enforce capitalization in header-entry for sensitive routers. See issue #1696
	req.Header.Set("Connection", "Close")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	Logger.Println("SOAP Request URL: " + url)
	Logger.Println("SOAP Action: " + req.Header.Get("SOAPAction"))
	Logger.Println("SOAP Request:\n\n" + body)

	r, err := http.DefaultClient.Do(req)
	if err != nil {
		Logger.Println(err)
		return resp, err
	}

	resp, _ = ioutil.ReadAll(r.Body)
	Logger.Printf("SOAP Response: %s\n\n%s\n\n", r.Status, resp)

	r.Body.Close()

	if r.StatusCode >= 400 {
		return resp, errors.New(function + ": " + r.Status)
	}

	return resp, nil
}

type soapGetExternalIPAddressResponseEnvelope struct {
	XMLName xml.Name
	Body    soapGetExternalIPAddressResponseBody `xml:"Body"`
}

type soapGetExternalIPAddressResponseBody struct {
	XMLName                      xml.Name
	GetExternalIPAddressResponse getExternalIPAddressResponse `xml:"GetExternalIPAddressResponse"`
}

type getExternalIPAddressResponse struct {
	NewExternalIPAddress string `xml:"NewExternalIPAddress"`
}

type soapErrorResponse struct {
	ErrorCode        int    `xml:"Body>Fault>detail>UPnPError>errorCode"`
	ErrorDescription string `xml:"Body>Fault>detail>UPnPError>errorDescription"`
}
