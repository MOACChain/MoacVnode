package config

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/MOACChain/MoacLib/log"
)

// Configuration to support the info in vnodeconfig.json
type Configuration struct {
	// Name   string `json:"name"`
	SCSService             bool   `json:SCSservice`
	ShowToPublic           bool   `json:ShowToPublic`
	VnodeServiceCfg        string `json:VnodeServiceCfg`
	VnodeBeneficialAddress string `json:VnodeBeneficialAddress`
	VnodeIP                string `json:"ip"`
	// ip                     *string //
	ForceSubnetP2P []string `json:"ForceSubnetP2P"`
}

// GetConfiguration: read config from .json file
// 1) No config file, using default value, don't create new file;
// 2) has config file, error in reading config, stop and display correct info;
// 3) has config file, no error in reading config, go ahead load and run;
func GetConfiguration(configFilePath string) (*Configuration, error) {
	//check the file path
	filePath, _ := filepath.Abs(configFilePath)
	log.Debugf("load vnode config file from %v", filePath)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		//If no config file exists, return nil
		log.Infof("%v not exists\nUse default settings", configFilePath)
		return nil, nil
	}

	if _, err := os.Stat(configFilePath); err != nil {

		log.Errorf("Open %v error: \n%v\n", configFilePath, err)

		return nil, err
	}

	return GetUserConfig(configFilePath)
}

//GetUserConfig -- read the user configuration in json file
func GetUserConfig(filepath string) (*Configuration, error) {
	// Open our jsonFile
	jsonFile, err := os.Open(filepath)
	// if we os.Open returns an error then handle it
	if err != nil {
		return nil, err
	}

	fmt.Println("Successfully Opened ", filepath)
	// defer the closing of our jsonFile so that we can parse it later on
	defer jsonFile.Close()

	// read our opened xmlFile as a byte array.
	byteValue, _ := ioutil.ReadAll(jsonFile)

	// initialize the Confgiruation array
	var conf Configuration

	// we unmarshal our byteArray which contains our
	// jsonFile's content into 'users' which we defined above
	json.Unmarshal(byteValue, &conf)

	return &conf, nil
}

//SaveUserConifg -- Save the input configuration in the json file
//discarded -
func SaveUserConifg(outconfig *Configuration) bool {

	filepath, _ := filepath.Abs("./vnodeconfig.json")

	outfile, ferr := os.Create(filepath)
	defer outfile.Close()

	if ferr != nil {
		//Cannot open the vnodeconfig.json file using the input filepath, save to default
		log.Infof("Cannot open %v configuration file!", filepath)
		return false
	}
	log.Infof("Save VNODE configuration to %v!", filepath)
	outjson := ""
	if outconfig.VnodeBeneficialAddress == "" {
		outjson = fmt.Sprintf("{\n  \"SCSService\":%v,\n  \"ShowToPublic\":%v,\n  \"VnodeServiceCfg\":\"%v\",\n  \"ip\":\"%v\",\n  \"VnodeBeneficialAddress\":\"\"\n}", outconfig.SCSService, outconfig.ShowToPublic, outconfig.VnodeServiceCfg, outconfig.VnodeIP)
	} else {
		outjson = fmt.Sprintf("{\n  \"SCSService\":%v,\n  \"ShowToPublic\":%v,\n  \"VnodeServiceCfg\":\"%v\",\n  \"ip\":\"%v\",\n  \"VnodeBeneficialAddress\":\"%v\"\n}", outconfig.SCSService, outconfig.ShowToPublic, outconfig.VnodeServiceCfg, outconfig.VnodeIP, outconfig.VnodeBeneficialAddress)
	}

	_, err := outfile.WriteString(outjson)
	if err != nil {
		log.Info("Write output configuration file Error:", err)
		return false
	}
	return true
}
