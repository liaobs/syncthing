// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

// Adapted from https://github.com/jackpal/Taipei-Torrent/blob/dd88a8bfac6431c01d959ce3c745e74b8a911793/IGD.go
// Copyright (c) 2010 Jack Palevich (https://github.com/jackpal/Taipei-Torrent/blob/dd88a8bfac6431c01d959ce3c745e74b8a911793/LICENSE)

// Package upnp implements UPnP InternetGatewayDevice discovery, querying, and port mapping.
package upnp

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
	"regexp"
	"strings"
	"time"

	"github.com/syncthing/syncthing/internal/sync"
)

// A container for relevant properties of a UPnP InternetGatewayDevice.
type IGD struct {
	uuid           string
	friendlyName   string
	services       []IGDService
	url            *url.URL
	localIPAddress string
}

// The InternetGatewayDevice's UUID.
func (n *IGD) UUID() string {
	return n.uuid
}

// The InternetGatewayDevice's friendly name.
func (n *IGD) FriendlyName() string {
	return n.friendlyName
}

// The InternetGatewayDevice's friendly identifier (friendly name + IP address).
func (n *IGD) FriendlyIdentifier() string {
	return "'" + n.FriendlyName() + "' (" + strings.Split(n.URL().Host, ":")[0] + ")"
}

// The URL of the InternetGatewayDevice's root device description.
func (n *IGD) URL() *url.URL {
	return n.url
}

// A container for relevant properties of a UPnP service of an IGD.
type IGDService struct {
	serviceID  string
	serviceURL string
	serviceURN string
}

func (s *IGDService) ID() string {
	return s.serviceID
}

type Protocol string

const (
	TCP Protocol = "TCP"
	UDP          = "UDP"
)

type upnpService struct {
	ServiceID   string `xml:"serviceId"`
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
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
func Discover(timeout time.Duration) []IGD {
	var results []IGD
	l.Infoln("Starting UPnP discovery...")

	interfaces, err := net.Interfaces()
	if err != nil {
		l.Infoln("Listing network interfaces:", err)
		return results
	}

	resultChan := make(chan IGD, 16)

	// Aggregator
	go func() {
	next:
		for result := range resultChan {
			for _, existingResult := range results {
				if existingResult.uuid == result.uuid {
					if debug {
						l.Debugf("Skipping duplicate result %s with services:", result.uuid)
						for _, svc := range result.services {
							l.Debugf("* [%s] %s", svc.serviceID, svc.serviceURL)
						}
					}
					goto next
				}
			}
			results = append(results, result)
			if debug {
				l.Debugf("UPnP discovery result %s with services:", result.uuid)
				for _, svc := range result.services {
					l.Debugf("* [%s] %s", svc.serviceID, svc.serviceURL)
				}
			}
		}
	}()

	wg := sync.NewWaitGroup()
	for _, intf := range interfaces {
		for _, deviceType := range []string{"urn:schemas-upnp-org:device:InternetGatewayDevice:1", "urn:schemas-upnp-org:device:InternetGatewayDevice:2"} {
			wg.Add(1)
			go func(intf net.Interface, deviceType string) {
				discover(&intf, deviceType, timeout, resultChan)
				wg.Done()
			}(intf, deviceType)
		}
	}

	wg.Wait()
	close(resultChan)

	suffix := "devices"
	if len(results) == 1 {
		suffix = "device"
	}

	l.Infof("UPnP discovery complete (found %d %s).", len(results), suffix)

	return results
}

// Search for UPnP InternetGatewayDevices for <timeout> seconds, ignoring responses from any devices listed in knownDevices.
// The order in which the devices appear in the result list is not deterministic
func discover(intf *net.Interface, deviceType string, timeout time.Duration, results chan<- IGD) {
	ssdp := &net.UDPAddr{IP: []byte{239, 255, 255, 250}, Port: 1900}

	tpl := `M-SEARCH * HTTP/1.1
Host: 239.255.255.250:1900
St: %s
Man: "ssdp:discover"
Mx: %d

`
	searchStr := fmt.Sprintf(tpl, deviceType, timeout/time.Second)

	search := []byte(strings.Replace(searchStr, "\n", "\r\n", -1))

	if debug {
		l.Debugln("Starting discovery of device type " + deviceType + " on " + intf.Name)
	}

	socket, err := net.ListenMulticastUDP("udp4", intf, &net.UDPAddr{IP: ssdp.IP})
	if err != nil {
		if debug {
			l.Debugln(err)
		}
		return
	}
	defer socket.Close() // Make sure our socket gets closed

	err = socket.SetDeadline(time.Now().Add(timeout))
	if err != nil {
		l.Infoln(err)
		return
	}

	if debug {
		l.Debugln("Sending search request for device type " + deviceType + " on " + intf.Name)
	}

	_, err = socket.WriteTo(search, ssdp)
	if err != nil {
		l.Infoln(err)
		return
	}

	if debug {
		l.Debugln("Listening for UPnP response for device type " + deviceType + " on " + intf.Name)
	}

	// Listen for responses until a timeout is reached
	for {
		resp := make([]byte, 1500)
		n, _, err := socket.ReadFrom(resp)
		if err != nil {
			if e, ok := err.(net.Error); !ok || !e.Timeout() {
				l.Infoln("UPnP read:", err) //legitimate error, not a timeout.
			}
			break
		}
		igd, err := parseResponse(deviceType, resp[:n])
		if err != nil {
			l.Infoln("UPnP parse:", err)
			continue
		}
		results <- igd
	}
	if debug {
		l.Debugln("Discovery for device type " + deviceType + " on " + intf.Name + " finished.")
	}
}

func parseResponse(deviceType string, resp []byte) (IGD, error) {
	if debug {
		l.Debugln("Handling UPnP response:\n\n" + string(resp))
	}

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
		return IGD{}, errors.New("invalid IGD response: no location specified.")
	}

	deviceDescriptionURL, err := url.Parse(deviceDescriptionLocation)

	if err != nil {
		l.Infoln("Invalid IGD location: " + err.Error())
	}

	deviceUSN := response.Header.Get("USN")
	if deviceUSN == "" {
		return IGD{}, errors.New("invalid IGD response: USN not specified.")
	}

	deviceUUID := strings.TrimLeft(strings.Split(deviceUSN, "::")[0], "uuid:")
	matched, err := regexp.MatchString("[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}", deviceUUID)
	if !matched {
		l.Infoln("Invalid IGD response: invalid device UUID", deviceUUID, "(continuing anyway)")
	}

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

func localIP(url *url.URL) (string, error) {
	conn, err := net.Dial("tcp", url.Host)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	localIPAddress, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return "", err
	}

	return localIPAddress, nil
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
	for _, svc := range d.Services {
		if svc.ServiceType == serviceType {
			result = append(result, svc)
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
			[]string{"urn:schemas-upnp-org:service:WANIPConnection:2", "urn:schemas-upnp-org:service:WANPPPConnection:1"})

		result = append(result, descriptions...)
	} else {
		return result, errors.New("[" + rootURL + "] Malformed root device description: not an InternetGatewayDevice.")
	}

	if len(result) < 1 {
		return result, errors.New("[" + rootURL + "] Malformed device description: no compatible service descriptions found.")
	} else {
		return result, nil
	}
}

func getIGDServices(rootURL string, device upnpDevice, wanDeviceURN string, wanConnectionURN string, serviceURNs []string) []IGDService {
	var result []IGDService

	devices := getChildDevices(device, wanDeviceURN)

	if len(devices) < 1 {
		l.Infoln("[" + rootURL + "] Malformed InternetGatewayDevice description: no WANDevices specified.")
		return result
	}

	for _, device := range devices {
		connections := getChildDevices(device, wanConnectionURN)

		if len(connections) < 1 {
			l.Infoln("[" + rootURL + "] Malformed " + wanDeviceURN + " description: no WANConnectionDevices specified.")
		}

		for _, connection := range connections {
			for _, serviceURN := range serviceURNs {
				services := getChildServices(connection, serviceURN)

				if len(services) < 1 && debug {
					l.Debugln("[" + rootURL + "] No services of type " + serviceURN + " found on connection.")
				}

				for _, service := range services {
					if len(service.ControlURL) == 0 {
						l.Infoln("[" + rootURL + "] Malformed " + service.ServiceType + " description: no control URL.")
					} else {
						u, _ := url.Parse(rootURL)
						replaceRawPath(u, service.ControlURL)

						if debug {
							l.Debugln("[" + rootURL + "] Found " + service.ServiceType + " with URL " + u.String())
						}

						service := IGDService{serviceID: service.ServiceID, serviceURL: u.String(), serviceURN: service.ServiceType}

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
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("User-Agent", "syncthing/1.0")
	req.Header.Set("SOAPAction", fmt.Sprintf(`"%s#%s"`, service, function))
	req.Header.Set("Connection", "Close")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	if debug {
		l.Debugln("SOAP Request URL: " + url)
		l.Debugln("SOAP Action: " + req.Header.Get("SOAPAction"))
		l.Debugln("SOAP Request:\n\n" + body)
	}

	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return resp, err
	}

	resp, _ = ioutil.ReadAll(r.Body)
	if debug {
		l.Debugln("SOAP Response:\n\n" + string(resp) + "\n")
	}

	r.Body.Close()

	if r.StatusCode >= 400 {
		return resp, errors.New(function + ": " + r.Status)
	}

	return resp, nil
}

// Add a port mapping to all relevant services on the specified InternetGatewayDevice.
// Port mapping will fail and return an error if action is fails for _any_ of the relevant services.
// For this reason, it is generally better to configure port mapping for each individual service instead.
func (n *IGD) AddPortMapping(protocol Protocol, externalPort, internalPort int, description string, timeout int) error {
	for _, service := range n.services {
		err := service.AddPortMapping(n.localIPAddress, protocol, externalPort, internalPort, description, timeout)
		if err != nil {
			return err
		}
	}
	return nil
}

// Delete a port mapping from all relevant services on the specified InternetGatewayDevice.
// Port mapping will fail and return an error if action is fails for _any_ of the relevant services.
// For this reason, it is generally better to configure port mapping for each individual service instead.
func (n *IGD) DeletePortMapping(protocol Protocol, externalPort int) error {
	for _, service := range n.services {
		err := service.DeletePortMapping(protocol, externalPort)
		if err != nil {
			return err
		}
	}
	return nil
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

// Add a port mapping to the specified IGD service.
func (s *IGDService) AddPortMapping(localIPAddress string, protocol Protocol, externalPort, internalPort int, description string, timeout int) error {
	tpl := `<u:AddPortMapping xmlns:u="%s">
	<NewRemoteHost></NewRemoteHost>
	<NewExternalPort>%d</NewExternalPort>
	<NewProtocol>%s</NewProtocol>
	<NewInternalPort>%d</NewInternalPort>
	<NewInternalClient>%s</NewInternalClient>
	<NewEnabled>1</NewEnabled>
	<NewPortMappingDescription>%s</NewPortMappingDescription>
	<NewLeaseDuration>%d</NewLeaseDuration>
	</u:AddPortMapping>`
	body := fmt.Sprintf(tpl, s.serviceURN, externalPort, protocol, internalPort, localIPAddress, description, timeout)

	_, err := soapRequest(s.serviceURL, s.serviceURN, "AddPortMapping", body)
	if err != nil {
		return err
	}

	return nil
}

// Delete a port mapping from the specified IGD service.
func (s *IGDService) DeletePortMapping(protocol Protocol, externalPort int) error {
	tpl := `<u:DeletePortMapping xmlns:u="%s">
	<NewRemoteHost></NewRemoteHost>
	<NewExternalPort>%d</NewExternalPort>
	<NewProtocol>%s</NewProtocol>
	</u:DeletePortMapping>`
	body := fmt.Sprintf(tpl, s.serviceURN, externalPort, protocol)

	_, err := soapRequest(s.serviceURL, s.serviceURN, "DeletePortMapping", body)

	if err != nil {
		return err
	}

	return nil
}

// Query the IGD service for its external IP address.
// Returns nil if the external IP address is invalid or undefined, along with any relevant errors
func (s *IGDService) GetExternalIPAddress() (net.IP, error) {
	tpl := `<u:GetExternalIPAddress xmlns:u="%s" />`

	body := fmt.Sprintf(tpl, s.serviceURN)

	response, err := soapRequest(s.serviceURL, s.serviceURN, "GetExternalIPAddress", body)

	if err != nil {
		return nil, err
	}

	envelope := &soapGetExternalIPAddressResponseEnvelope{}
	err = xml.Unmarshal(response, envelope)
	if err != nil {
		return nil, err
	}

	result := net.ParseIP(envelope.Body.GetExternalIPAddressResponse.NewExternalIPAddress)

	return result, nil
}
