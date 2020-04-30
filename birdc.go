package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/bamiaux/iobit"
)

type bgpCommunity struct {
	AS   uint16
	Data uint16
}

var birdCommunityRegex = regexp.MustCompile(`\((\d+,\d+)\)`)

var monitoredPrefix = flag.String(
	"peerprefix", "1.1.1.0/24", "the prefix of the other side")

var communityAS = flag.Int("communityASN", 23456,
	"The shared community AS used to communicate on")

var templatePath = flag.String("templateFile", "/etc/bird/conf.orig",
	"Where to find the template file")

var configPath = flag.String("confFile", "/etc/bird/bird.conf",
	"Where to write config file")

var sockPath = flag.String("sockFile", "/run/bird/bird.ctl",
	"Where to write config file")

/*
Three Communities are used:


Type 1: Game counter, Moves up
on each move, so that peers know
when a move happens.

T = Type
Z = Counter number

+-------------------------------+
|T|T|Z|Z|Z|Z|Z|Z|Z|Z|Z|Z|Z|Z|Z|Z|
+-------------------------------+

Type 2: Attack Cords

T = Type
X = X Cords
Y = Y Cords
S = Hit or Miss on last move

+-------------------------------+
|T|T|X|X|X|X|-|-|Y|Y|Y|Y|S|S|-|-|
+-------------------------------+
*/

func numberToBitReader(in uint16) iobit.Reader {
	actually := uint16(in)

	bits2 := make([]byte, 2)

	binary.BigEndian.PutUint16(bits2, actually)
	return iobit.NewReader(bits2)
}

var errNotEnoughData = fmt.Errorf("Not enough data to make a move")
var errInvalidType = fmt.Errorf("Invalid community type found")
var errDupeType = fmt.Errorf("Duplicate data read")

func readBGP() (gameIncrementor, X, Y, HitOrMissOnLast int, err error) {
	communities := readCommunities(*monitoredPrefix)

	readCounter, readPosition := false, false

	for _, community := range communities {
		if community.AS == uint16(*communityAS) {
			// okay, so we are now interested!
			r := numberToBitReader(community.Data)
			t := r.Uint8(2)

			if t == 1 {
				// Counter
				if readCounter {
					// uh we have read it twice, oh dear?
					return 0, 0, 0, 0, errDupeType
				}
				readCounter = true
				c := r.Uint16(14)
				gameIncrementor = int(c)

			} else if t == 2 {
				if readPosition {
					// uh we have read it twice, oh dear?
					return 0, 0, 0, 0, errDupeType
				}
				readPosition = true
				xp := r.Uint16(4)
				X = int(xp)
				r.Skip(2)
				yp := r.Uint16(4)
				Y = int(yp)
				hs := r.Uint16(2)
				HitOrMissOnLast = int(hs)

			} else {
				return 0, 0, 0, 0, errInvalidType
			}
		}
	}

	if readCounter && readPosition {
		return gameIncrementor, X, Y, HitOrMissOnLast, nil
	}
	return 0, 0, 0, 0, errNotEnoughData
}

func testBGPCode() {
	for x := 0; x < 10; x++ {
		for y := 0; y < 10; y++ {
			_, c2 := genCommunities(1, x, y, 0)

			r := numberToBitReader(c2)
			t := r.Uint8(2)
			if t != 2 {
				log.Printf("WTF??")
			}
			xp := r.Uint16(4)
			X := int(xp)
			r.Skip(2)
			yp := r.Uint16(4)
			Y := int(yp)
			// hs := r.Uint16(2)
			if X != x {
				fmt.Printf("Logic error X: Got %d != Sent %d\n", X, x)
			}

			if Y != y {
				fmt.Printf("Logic error Y: Got %d != Sent %d\n", Y, y)
			}
		}
	}
}

func genCommunities(gameIncrementor, X, Y, HitOrMissOnLast int) (uint16, uint16) {
	counternumberbytes := make([]byte, 2)
	counternumberbits := iobit.NewWriter(counternumberbytes)

	counternumberbits.PutUint16(2, 1)
	counternumberbits.PutUint16(14, uint16(gameIncrementor))
	counternumberbits.Flush()

	counterCommunity := binary.BigEndian.Uint16(counternumberbytes)

	positionnumberbytes := make([]byte, 2)
	positionnumberbits := iobit.NewWriter(positionnumberbytes)

	positionnumberbits.PutUint16(2, 2)
	positionnumberbits.PutUint16(4, uint16(X))
	positionnumberbits.PutUint16(2, 0) // pad
	positionnumberbits.PutUint16(4, uint16(Y))
	positionnumberbits.PutUint16(2, uint16(HitOrMissOnLast))
	positionnumberbits.PutUint16(2, 0) // pad
	positionnumberbits.Flush()

	positionCommunity := binary.BigEndian.Uint16(positionnumberbytes)
	return counterCommunity, positionCommunity
}

func writeBGP(gameIncrementor, X, Y, HitOrMissOnLast int) error {
	counterCommunity, positionCommunity :=
		genCommunities(gameIncrementor, X, Y, HitOrMissOnLast)

	// Now we have the two community strings counterCommunity and positionCommunity

	templatestring := fmt.Sprintf(
		"\nbgp_community.add((%d,%d));\nbgp_community.add((%d,%d));\n",
		*communityAS, positionCommunity, *communityAS, counterCommunity)

	templateBytes, err := ioutil.ReadFile(*templatePath)
	if err != nil {
		return err
	}

	birdConfigOutput := strings.Replace(string(templateBytes),
		"###COMMUNITY###", templatestring, 1)

	err = ioutil.WriteFile("/etc/bird/bird.conf", []byte(birdConfigOutput), 0640)
	if err != nil {
		return err
	}

	// now reload bird
	conn, err := net.Dial("unix", *sockPath)
	if err != nil {
		log.Fatalf("Unable to connect to bird %s", err.Error())
	}
	buffer := make([]byte, 90000)
	conn.Read(buffer)
	defer conn.Close()

	conn.Write([]byte(fmt.Sprintf("configure\n")))

	buffer = make([]byte, 90000)
	_, err = conn.Read(buffer)

	return err
}

func resetBird() error {

	templateBytes, err := ioutil.ReadFile(*templatePath)
	if err != nil {
		return err
	}

	birdConfigOutput := strings.Replace(string(templateBytes),
		"###COMMUNITY###", "", 1)

	err = ioutil.WriteFile(*configPath, []byte(birdConfigOutput), 0640)
	if err != nil {
		return err
	}

	// now reload bird
	conn, err := net.Dial("unix", *sockPath)
	if err != nil {
		log.Fatalf("Unable to connect to bird %s", err.Error())
	}
	buffer := make([]byte, 90000)
	conn.Read(buffer)
	defer conn.Close()

	conn.Write([]byte(fmt.Sprintf("configure\n")))

	buffer = make([]byte, 90000)
	_, err = conn.Read(buffer)

	return err
}

func readCommunities(prefix string) (o []bgpCommunity) {
	conn, err := net.Dial("unix", *sockPath)
	if err != nil {
		log.Fatalf("Unable to connect to bird %s", err.Error())
	}
	buffer := make([]byte, 90000)
	conn.Read(buffer)

	defer conn.Close()

	conn.Write([]byte(fmt.Sprintf("show route all %s\n", prefix)))

	buffer = make([]byte, 90000)
	n, err := conn.Read(buffer)

	if err != nil {
		log.Fatalf("Unable to read from bird %s", err.Error())
	}

	matches :=
		birdCommunityRegex.FindAllStringSubmatch(string(buffer[:n]), -1)

	o = make([]bgpCommunity, 0)

	for _, v := range matches {
		if len(v) == 2 {
			bits := strings.Split(v[1], ",")
			as, _ := strconv.ParseInt(bits[0], 10, 64)
			data, _ := strconv.ParseInt(bits[1], 10, 64)
			o = append(o, bgpCommunity{
				AS:   uint16(as),
				Data: uint16(data),
			})
		}
	}

	return o
}
