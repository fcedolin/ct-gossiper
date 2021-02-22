package main

import (
	"fmt"
	"net/http"
	"encoding/json"
	"os"
	"os/signal"
	"syscall"
	"bytes"
	"io/ioutil"
	"strings"
	"flag"
	"github.com/golang/glog"
	cto "ct-gossiper"
	mtrUtils "github.com/n-ct/ct-monitor/utils"
	mtr "github.com/n-ct/ct-monitor/entitylist"
)

var peers []*mtr.Monitor;

var messages cto.MessagesMap; //[TypeID][subjectOrSigner][Timestamp][Version]
var alertsMap cto.MessagesMap;//[Subject][Signer][Timestamp][Version]
var port string;
var allMonitors *mtr.MonitorsList;


func main() {

	done := make(chan os.Signal, 1); //create a channel to signify when server is shut down with ctrl+c
  signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM);//notify the channel when program terminated

	go func() {
		<-done
		glog.Infoln("kill recived");
		glog.Flush();
		os.Exit(1);
	}(); //when channel is notified print debug info, make sure all logs get written and exit

	//Setting flags
  var configFilename = flag.String("config", "", "File containing gossiper configuration");
	var monitorsFilename = flag.String("monitor_list", "", "File containing monitor-gossiper pairs");

	flag.Parse();
	defer glog.Flush(); //if the program ends unexpectedly make sure all debug info is printed

	//if filenames are not defined, terminate
	if(len(*configFilename) == 0 || len(*monitorsFilename) == 0){
    fmt.Println("configuration files are required.");
    return;
  }

	messages = make(cto.MessagesMap);
	alertsMap = make(cto.MessagesMap);

	gossiperSetup(*configFilename, *monitorsFilename);

	http.HandleFunc("/ct/v1/gossip", GossipHandler); // call GossipHandler on post to /gossip

	glog.Infof("Starting server on %v\n", port);

	err := http.ListenAndServe(fmt.Sprintf(":%v", port), nil); // start server
	if err != nil {
		fmt.Errorf("err: %v", err);
	}
}

// GossipHandler is called on a post request to /ct/v1/gossip.
// It handles the logic of gossip within a network system
func GossipHandler(w http.ResponseWriter, req *http.Request){
	data := cto.CTObject{};
	err := json.NewDecoder(req.Body).Decode(&data); // fill that struct using the JSON encoded struct send via the post
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest) // if there is an eror report and abort
		return;
	}

	//Get data identifier and select map to use
	identifier := data.Identifier();
	var workingMap cto.MessagesMap;
	if data.TypeID == "Alert" {
		workingMap = alertsMap;
	} else {
		workingMap = messages;
	}

	glog.Infof("CTObject recived from source: %s\n\n", data.ToDebugString()); //print contents of message (for debugging)
	if message, ok := workingMap[identifier.First][identifier.Second][identifier.Third][identifier.Fourth]; ok { // if I have the message already check for conflict
		if bytes.Compare(data.Hash, message.Hash)==0 {
			http.Error(w, "Duplicate item\n", http.StatusBadRequest); // if no conflic send back "duplicate item", and bad request status code to sender
		} else {
			glog.Infof("Misbehavior detected\n\n"); // if conflict send a PoM to all peers
			PoM := cto.NewCTObject("PoM", 0, []byte{0,1,2,3}, ""); //new dummy Proof of misbehavior
			addEntry(messages, *PoM, PoM.Identifier()); // store PoM

			//gossip new PoM to all peers
			for _, peer := range peers{
				glog.Infof("Gossiping PoM to peer: %v\n", peer.Id);
				post(fmt.Sprintf("%v/ct/v1/gossip", peer.Gossip), PoM, false);
			}
		}
	}else{
		if data.Blob == nil{ //If the message does not contain the blob
			fmt.Fprintf(w, "blob-request"); //respond with "blob-request"
		} else {
			fmt.Fprintf(w, "new data"); //respond with "new data"
			addEntry(workingMap, data, identifier);// if message is new add it to messages map

			for _, peer := range peers{
				glog.Infof("Gossiping new info to peer: %v\n", peer.Id);
				post(fmt.Sprintf("%v/ct/v1/gossip", peer.Gossip), &data, false);
			}
		}
	}
}

// post takes in an address as a string and a pointer to a CTObject struct
// and makes a post request to that address with the JSON encoded version of that struct
func post(address string, data *cto.CTObject, withBlob bool){
	var toSend *cto.CTObject;
	if withBlob {
		toSend = data;
	} else {
		toSend = data.CopyWithoutBlob();
	}
	var jsonStr, _ = json.Marshal(toSend);

	req, err := http.NewRequest("POST", address, bytes.NewBuffer(jsonStr)); //create a post request
	req.Header.Set("X-Custom-Header", "myvalue");
	req.Header.Set("Content-Type", "application/json"); //set message type to JSON

	client := &http.Client{};
	resp, err := client.Do(req); //make the request
	if err != nil {
		panic(err);
	}

	defer resp.Body.Close();

	//print info for debug
	glog.Infoln("response Status:", resp.Status);
	glog.Infoln("response Headers:", resp.Header);
	body, _ := ioutil.ReadAll(resp.Body);
	sbody := string(body);
	glog.Infoln("response Body:", sbody);

	if strings.ToLower(sbody) == "blob-request" {
		glog.Infof("sending blob to peer: %v\n\n", address);
		post(address, data, true); // if the recipient sends back a blob request resend the message with the blob
	}
}

//addEntry adds a new entry to the selected map using the data identifier as keys
func addEntry(dataMap cto.MessagesMap, data cto.CTObject, identifier cto.ObjectIdentifier){
	if _, ok := dataMap[identifier.First]; !ok {
		dataMap[identifier.First] = make(map[string]map[uint64]map[string] *cto.CTObject);
	}
	if _, ok := dataMap[identifier.First][identifier.Second]; !ok {
		dataMap[identifier.First][identifier.Second] = make(map[uint64]map[string] *cto.CTObject);
	}
	if _, ok := dataMap[identifier.First][identifier.Second][identifier.Third]; !ok {
		dataMap[identifier.First][identifier.Second][identifier.Third] = make(map[string] *cto.CTObject);
	}
	dataMap[identifier.First][identifier.Second][identifier.Third][identifier.Fourth] = &data;
}

//gossiperSetup configures gossiper varialbes from json files
func gossiperSetup(configFilename string, monitorsFilename string){
	gossipConfig := cto.NewGossipConfig(configFilename);

	getPeers(gossipConfig, monitorsFilename);
	port = strings.Split(allMonitors.FindMonitorByID(gossipConfig.Monitor_id).Gossip, ":")[2];

}

//getPeers get all monitors from file and populates the peers with the respective monitors
func getPeers(gossipConfig cto.GossipConfig, monitorsFilename string)  {
	allMonitors = mtr.NewMonitorList(monitorsFilename)

	for _, monitorId := range gossipConfig.Monitors_ids {
		peers = append(peers, allMonitors.FindMonitorByID(monitorId));
	}
}