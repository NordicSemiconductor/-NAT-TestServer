package main

import (
	"log"
	"net"
	"time"
	"encoding/json"
	"strconv"
	"errors"
	"github.com/xeipuuv/gojsonschema"
	"path/filepath"
	"strings"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
    "github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/google/uuid"
	"context"
	"os"
)

type Packet struct{
	Operator string `json:"op"`
	IP string `json:"ip"`
	CellId int `json:"cell_id"`
	UEMode int `json:"ue_mode"`
	ICCID string `json:"iccid"`
	Interval int `json:"interval"`
}

type SaveData struct {
	Received string
	Protocol string
	IP string
	Timeout bool
	Data Packet
}

type SaveRoutineStruct struct {
	Timestamp time.Time 
	Data SaveData
}

const UDPport = ":3050"
const TCPport = ":3051"
const newPacketTimeout = 10
const maxBufferSize = 256
const schemaFile = "schema.json"
const timeFormat = "2006-01-02T15:04:05.00-0700"
var dcBuffer []byte = []byte("Error occured.\nConnection closed.\n")
var saveChan chan SaveRoutineStruct
var schemaLoader gojsonschema.JSONLoader

func SaveRoutine(){
	awsBucket := os.Getenv("AWS_BUCKET")
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(os.Getenv("AWS_REGION"))},
	)
	if err != nil {
		log.Fatal("Error creating session ", err)
	} 
	svc := s3.New(sess, &aws.Config{
		Region: aws.String(os.Getenv("AWS_REGION"))},
	)

	for i := range saveChan {
		buffer, err := json.Marshal(i.Data)
		if  err != nil {
			log.Printf("JSON invalid. Cannot write to file, error:%d\n", err)
		}

		newUUID, err := uuid.NewRandom()
		if err != nil {}

		key := fmt.Sprintf("%s/%s-%s-%s.json", i.Timestamp.Format("2006/01/02"), i.Data.IP, i.Timestamp.Format("150405"), newUUID)
		
		ctx := context.Background()
		var cancelFn func()
		if newPacketTimeout > 0 {
			ctx, cancelFn = context.WithTimeout(ctx, newPacketTimeout * time.Second)
		}

		go func() {
			_, err = svc.PutObjectWithContext(ctx, &s3.PutObjectInput{
				Bucket: aws.String(awsBucket),
				Key:    aws.String(key),
				Body:   strings.NewReader(string(buffer)),
			})
			if err != nil {
				if aerr, ok := err.(awserr.Error); ok && aerr.Code() == request.CanceledErrorCode {
					log.Printf("Upload canceled due to timeout, %s\n", err.Error())
				} else {
					log.Printf("Failed to upload object, %s\n", err.Error())
				}
			}
		
			if cancelFn != nil {
				cancelFn()
			}
		}()
	}
}

func HandleData(buffer []byte, protocol string, addr string) ([]byte, SaveRoutineStruct, error) {
	startTime := time.Now()

	documentLoader := gojsonschema.NewStringLoader(string(buffer))
	result, err := gojsonschema.Validate(schemaLoader, documentLoader)
	if err != nil {
		return nil, SaveRoutineStruct{}, err
	} else if !result.Valid() {
		return nil, SaveRoutineStruct{}, errors.New("Wrong format in packet")
	}

	var packet Packet
	err = json.Unmarshal(buffer, &packet)
	if err != nil {
		return nil, SaveRoutineStruct{}, err
	}

	saveData := SaveRoutineStruct{Timestamp: startTime, Data: SaveData{Received: startTime.Format(timeFormat), Protocol: protocol, IP: addr, Timeout: false, Data: packet}}
	saveChan <- saveData

	time.Sleep(time.Duration(packet.Interval)*time.Second)

	endTime := time.Now().Format(timeFormat)
	retString := "Interval: " + strconv.Itoa(packet.Interval) + "\nReceived:" + startTime.Format(timeFormat) + "\nReturned: " + endTime +"\n";
	return []byte(retString), saveData, nil
}

func HandleUDP(pc net.PacketConn,addr net.Addr, buffer []byte){
	retBuffer, saveData, err := HandleData(buffer, "UDP", addr.String())
	if err != nil {
		log.Printf("Invalid UDP Packet received from %s. Connection terminated.\n", addr.String())
		_, err = pc.WriteTo(dcBuffer, addr)
		if err != nil {}
		return
	}

	log.Printf("UDP Packet received from %s\n", addr.String())

	_, err = pc.WriteTo(retBuffer, addr)
	if err != nil {
		log.Printf("UDP write to %s failed, error: %s\n", addr.String(), err.Error())
		saveData.Data.Timeout = true
		saveChan <- saveData
		return
	}
	log.Printf("Packet sent to %s\n", addr.String())
}

func HandleTCP(conn net.Conn){
	for {
		buffer := make([]byte, maxBufferSize)
		
		n, err := conn.Read(buffer)
		if err != nil {
			_, err = conn.Write(dcBuffer)
			if err != nil {}
			conn.Close()
			return
		}

		retBuffer, saveData, err := HandleData(buffer[:n-1], "TCP", conn.RemoteAddr().String())
		if err != nil {
			log.Printf("Invalid TCP Packet received from %s. Connection terminated.\n", conn.RemoteAddr().String())
			_, err = conn.Write(dcBuffer)
			if err != nil {}
			conn.Close()
			return
		}

		log.Printf("TCP Packet received from %s\n", conn.RemoteAddr().String())
		
		_, err = conn.Write(retBuffer)
		if err != nil {
			log.Printf("TCP write to %s failed. Connection terminated\n", conn.RemoteAddr().String())
			saveData.Data.Timeout = true
			saveChan <- saveData
			conn.Close()
			return
		}
		log.Printf("Packet sent to %s\n", conn.RemoteAddr().String())
	}
}

func AcceptUDP(pc net.PacketConn){
	for {
		buffer := make([]byte, maxBufferSize)

		n, addr, err := pc.ReadFrom(buffer)
		if err != nil {
			continue
		}

		go HandleUDP(pc, addr, buffer[:n-1])
	}
}

func AcceptTCP(l net.Listener){
	for {
		conn, err := l.Accept()
		if err != nil {
			continue
		}
		
		go HandleTCP(conn)
	}
}

func main(){
	done := make(chan bool)
	saveChan = make(chan SaveRoutineStruct)

	absPath, _ := filepath.Abs(schemaFile)
	absPath = "file:///" + strings.ReplaceAll(absPath, "\\", "/")
	schemaLoader = gojsonschema.NewReferenceLoader(absPath)

	pc, err := net.ListenPacket("udp", UDPport)
	if err != nil {
		log.Fatal(err)
	}

    l, err := net.Listen("tcp", TCPport)
    if err != nil {
        log.Fatal(err)
	}

	defer pc.Close()
	defer l.Close()
	
	go AcceptUDP(pc)
	go AcceptTCP(l)
	go SaveRoutine()

	<-done
}
