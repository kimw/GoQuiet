// +build go1.8,!go1.10

package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/cbeuw/GoQuiet/gqclient"
	"github.com/cbeuw/GoQuiet/gqclient/TLS"
	"github.com/cbeuw/gotfo"
	logging "github.com/op/go-logging"
)

var version string

// ss refers to the ss-client, remote refers to the proxy server

type pipe interface {
	remoteToSS()
	ssToRemote()
	closePipe()
}

type pair struct {
	ss     net.Conn
	remote net.Conn
}

func (p *pair) closePipe() {
	go p.ss.Close()
	go p.remote.Close()
}

func (p *pair) remoteToSS() {
	buf := make([]byte, 20480)
	for {
		i, err := gqclient.ReadTillDrain(p.remote, buf)
		if err != nil {
			p.closePipe()
			return
		}
		data := TLS.PeelRecordLayer(buf[:i])
		_, err = p.ss.Write(data)
		if err != nil {
			p.closePipe()
			return
		}
	}
}

func (p *pair) ssToRemote() {
	buf := make([]byte, 10240)
	for {
		i, err := io.ReadAtLeast(p.ss, buf, 1)
		if err != nil {
			p.closePipe()
			return
		}
		data := buf[:i]
		data = TLS.AddRecordLayer(data, []byte{0x17}, []byte{0x03, 0x03})
		_, err = p.remote.Write(data)
		if err != nil {
			p.closePipe()
			return
		}
	}
}

func initSequence(ssConn net.Conn, sta *gqclient.State) {
	// SS likes to make TCP connections and then immediately close it
	// without sending anything. This is apperently a feature.
	// But we don't want this because it may be significant to the GFW
	// and we don't want to make meaningless handshakes.
	// So we filter these empty connections
	var err error
	data := make([]byte, 1024)
	i, err := io.ReadAtLeast(ssConn, data, 1)
	if err != nil {
		go ssConn.Close()
	}
	data = data[:i]

	var remoteConn net.Conn
	clientHello := TLS.ComposeInitHandshake(sta)
	if sta.FastOpen {
		remoteConn, err = gotfo.Dial(sta.SS_REMOTE_HOST+":"+sta.SS_REMOTE_PORT, true, clientHello)
		if err != nil {
			log.Errorf("Connecting and sending ClientHello to remote: %v\n", err)
			return
		}
	} else {
		remoteConn, err = gotfo.Dial(sta.SS_REMOTE_HOST+":"+sta.SS_REMOTE_PORT, false, nil)
		if err != nil {
			log.Errorf("Connecting to remote: %v\n", err)
			return
		}
		_, err = remoteConn.Write(clientHello)
		if err != nil {
			log.Errorf("Sending ClientHello: %v\n", err)
			return
		}
	}

	// Three discarded messages: ServerHello, ChangeCipherSpec and Finished
	discardBuf := make([]byte, 1024)
	for c := 0; c < 3; c++ {
		_, err = gqclient.ReadTillDrain(remoteConn, discardBuf)
		if err != nil {
			log.Errorf("Reading discarded message %v: %v\n", c, err)
			return
		}
	}

	reply := TLS.ComposeReply()
	_, err = remoteConn.Write(reply)
	if err != nil {
		log.Errorf("Sending reply to remote: %v\n", err)
		return
	}
	p := pair{
		ssConn,
		remoteConn,
	}

	// Send the data we got from SS in the beginning
	data = TLS.AddRecordLayer(data, []byte{0x17}, []byte{0x03, 0x03})
	_, err = p.remote.Write(data)
	if err != nil {
		log.Errorf("Sending first SS data to remote: %v\n", err)
		p.closePipe()
		return
	}
	go p.remoteToSS()
	go p.ssToRemote()
}

var log *logging.Logger

func init() {
	// Initial logger
	log = logging.MustGetLogger("go-server")
	format := logging.MustStringFormatter(
		` %{color}%{time:2006-01-02 15:04:05} %{level}: %{shortfile}%{color:reset} %{message}`,
	)
	backend := logging.NewLogBackend(os.Stderr, "", 0)
	backendFormatter := logging.NewBackendFormatter(backend, format)
	logging.SetBackend(backendFormatter)
}

func main() {
	// Should be 127.0.0.1 to listen to ss-local on this machine
	var localHost string
	// server_port in ss config, ss sends data on loopback using this port
	var localPort string
	// The ip of the proxy server
	var remoteHost string
	// The proxy port,should be 443
	var remotePort string
	var pluginOpts string

	// These two functions do nothing for non-android
	log_init()
	protect()

	if os.Getenv("SS_LOCAL_HOST") != "" {
		localHost = os.Getenv("SS_LOCAL_HOST")
		localPort = os.Getenv("SS_LOCAL_PORT")
		remoteHost = os.Getenv("SS_REMOTE_HOST")
		remotePort = os.Getenv("SS_REMOTE_PORT")
		pluginOpts = os.Getenv("SS_PLUGIN_OPTIONS")
	} else {
		localHost = "127.0.0.1"
		flag.StringVar(&localPort, "l", "", "localPort: same as server_port in ss config, the plugin listens to SS using this")
		flag.StringVar(&remoteHost, "s", "", "remoteHost: IP of your proxy server")
		flag.StringVar(&remotePort, "p", "443", "remotePort: proxy port, should be 443")
		flag.StringVar(&pluginOpts, "c", "gqclient.json", "configPath: path to gqclient.json")
		askVersion := flag.Bool("v", false, "Print the version number")
		printUsage := flag.Bool("h", false, "Print this message")
		flag.Parse()

		if *askVersion {
			fmt.Printf("gq-client %s\n", version)
			return
		}

		if *printUsage {
			flag.Usage()
			return
		}

		log.Noticef("Starting standalone mode. Listening for ss on %v:%v\n", localHost, localPort)
	}

	opaque := gqclient.BtoInt(gqclient.CryptoRandBytes(32))
	sta := &gqclient.State{
		SS_LOCAL_HOST:  localHost,
		SS_LOCAL_PORT:  localPort,
		SS_REMOTE_HOST: remoteHost,
		SS_REMOTE_PORT: remotePort,
		Now:            time.Now,
		Opaque:         opaque,
	}
	err := sta.ParseConfig(pluginOpts)
	if err != nil {
		log.Fatal(err)
	}

	if sta.SS_LOCAL_PORT == "" {
		log.Fatal("Must specify localPort")
	}
	if sta.SS_REMOTE_HOST == "" {
		log.Fatal("Must specify remoteHost")
	}
	if sta.Key == "" {
		log.Fatal("Key cannot be empty")
	}
	if sta.TicketTimeHint == 0 {
		log.Fatal("TicketTimeHint cannot be empty or 0")
	}

	sta.SetAESKey()
	listener, err := gotfo.Listen(sta.SS_LOCAL_HOST+":"+sta.SS_LOCAL_PORT, sta.FastOpen)
	if err != nil {
		log.Fatal(err)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Error(err)
			continue
		}
		go initSequence(conn, sta)
	}
}
