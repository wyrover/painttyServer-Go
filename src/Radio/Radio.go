package Radio

import "time"

import "log"
import "Socket"
import "BufferedFile"
import "sync"

type RadioTaskList struct {
	tasks  []RadioChunk
	locker sync.Mutex
}

type RadioChunk interface {
	special() // in case others may mis-use this interface
}

type RAMChunk struct {
	Data []byte
}

type FileChunk struct {
	Start  int64
	Length int64
}

// ensure interface
func (c RAMChunk) special() {
	//
}

// ensure interface
func (c FileChunk) special() {
	//
}

type RadioClient struct {
	client    *Socket.SocketClient
	sendChan  chan RAMChunk
	writeChan chan FileChunk
	list      *RadioTaskList
}

type RadioSendPart struct {
	Data []byte
}

type RadioSingleSendPart struct {
	Data   []byte
	Client *Socket.SocketClient
}

type Radio struct {
	clients        map[*Socket.SocketClient]*RadioClient
	file           *BufferedFile.BufferedFile
	GoingClose     chan bool
	SingleSendChan chan RadioSingleSendPart
	SendChan       chan RadioSendPart
	WriteChan      chan RadioSendPart
	signature      string
	locker         sync.Mutex
}

func (r *Radio) Close() {
	close(r.GoingClose)
	close(r.SingleSendChan)
	close(r.SendChan)
	close(r.WriteChan)
}

func (r *Radio) Signature() string {
	return r.signature
}

func (r *Radio) Prune() string {
	r.locker.Lock()
	defer r.locker.Unlock()
	for _, v := range r.clients {
		v.list = &RadioTaskList{
			make([]RadioChunk, 0, 100),
			sync.Mutex{},
		}
	}
	if err := r.file.Clear(); err != nil {
		panic(err)
	}
	r.signature = genArchiveSign(r.signature)
	return r.signature
}

func (r *Radio) AddClient(client *Socket.SocketClient, start, length int64) {
	r.locker.Lock()
	defer r.locker.Unlock()

	var list = &RadioTaskList{
		make([]RadioChunk, 0),
		sync.Mutex{},
	}
	var fileSize = r.file.WholeSize()
	var startPos, chunkSize int64
	if start > fileSize {
		startPos = fileSize
	} else {
		startPos = start
	}
	if startPos+length > fileSize {
		chunkSize = length
	} else {
		chunkSize = fileSize - startPos
	}
	if chunkSize != 0 {
		var chunks = splitChunk(FileChunk{
			Start:  startPos,
			Length: chunkSize,
		})
		list.Append(chunks)
		log.Println("tasks assigned", list.Tasks())
	}
	var radioClient = RadioClient{
		client:    client,
		sendChan:  make(chan RAMChunk),
		writeChan: make(chan FileChunk),
		list:      list,
	}
	//log.Println("init tasks", radioClient.list)

	r.clients[client] = &radioClient

	go func() {
		for {
			//r.locker.Lock()
			//radioClient, ok := r.clients[client]
			//if !ok {
			//	r.RemoveClient(client)
			//	return
			//}
			//r.locker.Unlock()

			select {
			case _, _ = <-client.GoingClose:
				r.RemoveClient(client)
				return
			case chunk, ok := <-radioClient.sendChan:
				if ok {
					log.Println("send chan happened")
					appendToPendings(chunk, radioClient.list)
				} else {
					log.Println("send chan miss-matched")
					r.RemoveClient(client)
					return
				}
			case chunk, ok := <-radioClient.writeChan:
				if ok {
					log.Println("write chan happened")
					appendToPendings(chunk, radioClient.list)
				} else {
					log.Println("write chan miss-matched")
					r.RemoveClient(client)
					return
				}
			default:
				time.Sleep(time.Millisecond * 300)
				fetchAndSend(client, radioClient.list, r.file)
			}

		}
	}()
}

func (r *Radio) RemoveClient(client *Socket.SocketClient) {
	r.locker.Lock()
	defer r.locker.Unlock()
	log.Println("remove client from radio")
	delete(r.clients, client)
}

func (r *Radio) FileSize() int64 {
	return r.file.WholeSize()
}

// SingleSend expected Buffer that send to one specific Client but doesn't record.
func (r *Radio) singleSend(data []byte, client *Socket.SocketClient) {
	r.locker.Lock()
	defer r.locker.Unlock()

	cli, ok := r.clients[client]
	if !ok {
		return
	}
	go func() {
		select {
		case cli.sendChan <- RAMChunk{data}:
		case <-time.After(time.Second * 10):
			r.RemoveClient(client)
			log.Println("sendChan failed in singleSend")
		}
	}()
}

// Send expected Buffer that send to every Client but doesn't record.
func (r *Radio) send(data []byte) {
	r.locker.Lock()
	defer r.locker.Unlock()
	fun := func(client *Socket.SocketClient, cli *RadioClient) {
		defer func() { recover() }()
		select {
		case cli.sendChan <- RAMChunk{data}:
		case <-time.After(time.Second * 10):
			r.RemoveClient(client)
			log.Println("sendChan failed in send")
		}
	}
	for client, cli := range r.clients {
		go fun(client, cli)
	}
}

// Write expected Buffer that send to every Client and record data.
func (r *Radio) write(data []byte) {
	var oldPos = r.file.WholeSize()
	wrote, err := r.file.Write(data)
	log.Println("wrote", wrote, "into radio")
	if err != nil {
		panic(err)
	}
	fun := func(client *Socket.SocketClient, cli *RadioClient) {
		defer func() { recover() }() // in case cli.writeChan is closed
		select {
		case cli.writeChan <- FileChunk{
			Start:  oldPos,
			Length: int64(len(data)),
		}:
		case <-time.After(time.Second * 10):
			r.RemoveClient(client)
			log.Println("writeChan failed in write")
		}
	}
	r.locker.Lock()
	defer r.locker.Unlock()
	for client, cli := range r.clients {
		log.Println("published")
		go fun(client, cli)
	}
}

func (r *Radio) run() {
	for {
		select {
		case _, _ = <-r.GoingClose:
			return
		case part, ok := <-r.SendChan:
			if !ok {
				return
			}
			r.send(part.Data)
		case part, ok := <-r.SingleSendChan:
			if !ok {
				return
			}
			r.singleSend(part.Data, part.Client)
		case part, ok := <-r.WriteChan:
			if !ok {
				return
			}
			r.write(part.Data)
		}
	}
}

func MakeRadio(fileName string) (*Radio, error) {
	var file, err = BufferedFile.MakeBufferedFile(
		BufferedFile.BufferedFileOption{
			fileName,
			time.Second * 3,
			1024 * 100,
		})
	if err != nil {
		return &Radio{}, err
	}
	var radio = &Radio{
		clients:        make(map[*Socket.SocketClient]*RadioClient),
		file:           file,
		GoingClose:     make(chan bool),
		SingleSendChan: make(chan RadioSingleSendPart),
		SendChan:       make(chan RadioSendPart),
		WriteChan:      make(chan RadioSendPart),
		locker:         sync.Mutex{},
		signature:      fileName, // TODO: recovery
	}
	go radio.run()
	return radio, nil
}
