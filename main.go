package main

import (
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/asim/mq/go/client"
	"github.com/gorilla/websocket"
)

type mq struct {
	client client.Client

	sync.RWMutex
	topics map[string][]chan []byte
}

var (
	address = flag.String("address", ":8081", "MQ server address")
	cert    = flag.String("cert_file", "", "TLS certificate file")
	key     = flag.String("key_file", "", "TLS key file")
	proxy   = flag.Bool("proxy", false, "Proxy for an MQ cluster")
	servers = flag.String("servers", "", "Comma separated MQ cluster list used by Proxy")

	defaultMQ *mq
)

func init() {
	flag.Parse()

	if *proxy && len(*servers) == 0 {
		log.Fatal("Proxy enabled without MQ server list")
	}

	defaultMQ = &mq{
		client: client.New(client.WithServers(strings.Split(*servers, ",")...)),
		topics: make(map[string][]chan []byte),
	}
}

func Log(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s", r.RemoteAddr, r.Method, r.URL)
		handler.ServeHTTP(w, r)
	})
}

func (m *mq) pub(topic string, payload []byte) error {
	if *proxy {
		return m.client.Publish(topic, payload)
	}

	m.RLock()
	subscribers, ok := m.topics[topic]
	m.RUnlock()
	if !ok {
		return nil
	}

	go func() {
		for _, subscriber := range subscribers {
			select {
			case subscriber <- payload:
			default:
			}
		}
	}()

	return nil
}

func (m *mq) sub(topic string) (<-chan []byte, error) {
	if *proxy {
		return m.client.Subscribe(topic)
	}

	ch := make(chan []byte, 100)
	m.Lock()
	m.topics[topic] = append(m.topics[topic], ch)
	m.Unlock()
	return ch, nil
}

func (m *mq) unsub(topic string, sub <-chan []byte) error {
	if *proxy {
		// noop
		return nil
	}

	m.RLock()
	subscribers, ok := m.topics[topic]
	m.RUnlock()

	if !ok {
		return nil
	}

	var subs []chan []byte
	for _, subscriber := range subscribers {
		if subscriber == sub {
			continue
		}
		subs = append(subs, subscriber)
	}

	m.Lock()
	m.topics[topic] = subs
	m.Unlock()

	return nil
}

func pub(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("topic")
	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Pub error", http.StatusInternalServerError)
		return
	}
	r.Body.Close()

	err = defaultMQ.pub(topic, b)
	if err != nil {
		http.Error(w, "Pub error", http.StatusInternalServerError)
		return
	}
}

func sub(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Upgrade(w, r, w.Header(), 1024, 1024)
	if err != nil {
		log.Println("Failed to open websocket connection")
		http.Error(w, "Could not open websocket connection", http.StatusBadRequest)
		return
	}

	topic := r.URL.Query().Get("topic")
	ch, err := defaultMQ.sub(topic)
	if err != nil {
		log.Printf("Failed to retrieve event for %s topic", topic)
		http.Error(w, "Could not retrieve events", http.StatusInternalServerError)
		return
	}
	defer defaultMQ.unsub(topic, ch)

	for {
		select {
		case e := <-ch:
			if err = conn.WriteMessage(websocket.BinaryMessage, e); err != nil {
				log.Printf("error sending event: %v", err.Error())
				return
			}
		}
	}
}

func main() {
	// MQ Handlers
	http.HandleFunc("/pub", pub)
	http.HandleFunc("/sub", sub)

	if len(*cert) > 0 && len(*key) > 0 {
		log.Println("TLS Enabled")
		log.Println("MQ listening on", *address)
		http.ListenAndServeTLS(*address, *cert, *key, Log(http.DefaultServeMux))
		return
	}

	if *proxy {
		log.Println("Proxy enabled")
	}

	log.Println("MQ listening on", *address)
	http.ListenAndServe(*address, Log(http.DefaultServeMux))
}
