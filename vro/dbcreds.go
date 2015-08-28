// Copyright (c) 2015 VMware
// Author: Tom Hite (thite@vmware.com)
//
// License: MIT (see https://github.com/tdhite/go-reminders/LICENSE).
//
package vro

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"github.com/tdhite/go-reminders/db"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"time"
)

func (a *BasicAuth) getHttpRequest(method string, url string, body io.Reader) (
		*http.Client,
		*http.Request,
		error){

	client := &http.Client{}

	if a.insecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return client, req, err
	}

	req.SetBasicAuth(a.user, a.passwd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	return client, req, nil
}

func (a *BasicAuth) getUrl(url string) ([]byte, error) {
	if url == "" {
		return nil, errors.New("DB struct has insufficient auth information.")
	}

	client, req, err := a.getHttpRequest("GET", url, nil)
	if err != nil {
		log.Printf(err.Error())
		return nil, err
	}

	rsp, err := client.Do(req)
	if err != nil {
		log.Printf(err.Error())
		return nil, err
	}
	defer rsp.Body.Close()

	body, err := ioutil.ReadAll(rsp.Body)
	return body, nil
}

func (a *BasicAuth) postUrl(url string) (string, error) {
	if url == "" {
		return "", errors.New("empty post url.")
	}

	client, req, err := a.getHttpRequest("POST", url, bytes.NewBuffer([]byte("{}")))
	if err != nil {
		log.Printf(err.Error())
		return "", err
	}

	rsp, err := client.Do(req)
	if err != nil {
		log.Printf(err.Error())
		return "", err
	}
	defer rsp.Body.Close()

	// read and clear the body (empty in reality so superfluous)
	_, _ = ioutil.ReadAll(rsp.Body)

	// Pull polling location from headers
	location := rsp.Header.Get("Location")
	log.Printf("POST LOCATION: %s\n", location)

	return location, nil
}

func pjsonerror(body []byte, err error) {
	log.Printf("JSON value:\n%s\n", body)

	if err != nil {
		log.Printf("%T\n%s\n%#v\n", err, err, err)
		switch v := err.(type) {
		case *json.SyntaxError:
			log.Println(string(body[v.Offset-40 : v.Offset]))
		}
	}
}

// Pull the creds Url from the original executions. The JSON is
// somewhat heavy as it involves vRO input/output parameters, etc.
// Read the 'map' refitting carefully when modifying this code and
// assure it closely (obviously) follows the JSON.
func (a *BasicAuth) getvROCredsUrl(url string) (string, error) {
	location, err := a.postUrl(url)
	if err != nil {
		return "", nil
	}

	return location, nil
}

func extractCreds(body []byte) (string, error) {
	var m map[string]interface{}
	err := json.Unmarshal(body, &m)
	pjsonerror(body, nil)
	if err != nil {
		return "", err
	}

	// need to check the status of the running 'execution' to wait for complete
	var state string
	if s, ok := m["state"]; ok {
		state = s.(string)
	} else {
		return "", errors.New("Error: state not supplied by vRO.")
	}

	// if state is running, we just need to wait a while
	if state == "running" {
		return "", nil
	}

	// if state is not complete and not running, something bad happened
	if state != "completed" {
		return "", errors.New("Error: state returned " + state)
	}

	var outparams []interface{}
	if o, ok := m["output-parameters"]; ok {
		outparams = o.([]interface{})
	} else {
		log.Println("No output-parameters returned by vRO.")
		return "", nil
	}

	// output values look like: output-parameters { value { string  {value }}}
	var value map[string]interface{}
	if len(outparams) > 0 {
		v := outparams[0]
		value = v.(map[string]interface{})
		if v, ok := value["value"]; ok {
			value = v.(map[string]interface{})
			if s, ok := value["string"]; ok {
				value = s.(map[string]interface{})
				if creds, ok := value["value"]; ok {
					return creds.(string), nil
				}
			}
		}
	}

	return "", errors.New("Incorrectly reached end of extractCreds!")
}

func (a *BasicAuth) __getvROCredentials(credsUrl string, chanCreds chan vROCreds) error {
	body, err := a.getUrl(credsUrl)
	if err != nil {
		return err
	}

	creds, err := extractCreds(body)
	if err != nil {
		return err
	}

	if creds == "" {
		return nil // this is not an error, just not ready yet
	}

	log.Println("JSON RETURN: " + creds)

	var m map[string]string
	err = json.Unmarshal([]byte(creds), &m)
	if err != nil {
		pjsonerror([]byte(creds), err)
		return err
	}

	vrocreds := vROCreds{
		host:   m["Host"],
		port:   m["Port"],
		admin:  m["Admin"],
		passwd: m["Passwd"],
	}

	chanCreds <- vrocreds

	return nil
}

// Wrapper to the REST call for DB creds. The vRA implementation takes
// a bit to create the database, and the vRO bridge will not return credentials
// until it is ready. This function polls up to five minutes for the creds.
func (a *BasicAuth) getvROCredentials(credsUrl string) (vROCreds, error) {
	// setup five minute overall timeout
	timeout := make(chan bool, 1)
	go func() {
		time.Sleep(time.Minute * 5)
		timeout <- true
	}()

	var creds vROCreds
	var err error
	for {
		// call timer to wait across polls
		calltimer := make(chan bool, 1)
		go func() {
			time.Sleep(time.Second * 10)
			calltimer <- true
		}()

		// this is the signal that vRA is done
		chanCreds := make(chan vROCreds, 1)

		// poll vRO for the credentials
		err = a.__getvROCredentials(credsUrl, chanCreds)
		if err != nil {
			log.Printf("Exiting polling due to error. %v.", err)
			break
		}

		finished := false
		select {
		case creds = <-chanCreds:
			log.Println("Finished polling for vRO Credentials")
			close(chanCreds)
			finished = true
		case <-calltimer:
			log.Printf("Polling for creds, again, for db credentials.")
		case <-timeout:
			err = errors.New("Timed out getting vRO Credentials")
			log.Println(err.Error())
			close(chanCreds)
			finished = true
		}
		if finished {
			break
		}
	}

	return creds, err
}

func (a *BasicAuth) GetDBCredsBasicAuth(exeurl string, db *db.DB) error {
	credsUrl, err := a.getvROCredsUrl(exeurl)
	if err != nil {
		return err
	}

	creds, err := a.getvROCredentials(credsUrl)
	if err != nil {
		return err
	}

	port, err := strconv.Atoi(creds.port)
	if err != nil {
		log.Printf("Bad port value in vRO Credentials.")
		return err
	}

	db.SetAddress(creds.host)
	db.SetPort(port)
	db.SetAdmin(creds.admin)
	db.SetPasswd(creds.passwd)

	return err
}