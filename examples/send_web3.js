/*
 * Generate a transaction for mc transfer with web3 lib and EIP155
 * in the MOAC test network
 * for testing MOAC wallet server
 * Test conditions:
 * 1. a pair of address/private key for testing, address need to have some balances.
 *    need to update the transaction nonce after each TX.
 * 2. an address to send to.
 * 
*/
//library used to compare two results.
var chai = require('chai');
var assert = chai.assert;

//libraries to generate the Tx
const Web3 = require('web3');

const localhost = 'http://localhost:8545';
let web3 = new Web3(localhost);

//Call the function, note the input value is in 'mc'
//test accounts
//Need to add the addr and private key
var taccts = [{
  "addr": "", 
  "key": ""//put the private key here
},{
  "addr": "", 
  "key": ""
}];

var src = taccts[0];
var des = taccts[1];


var chainid = web3.eth.getChainId().then(function(inId){

  console.log("This TX is on network:", inId);
  sendTx(src, des, inId, 0.001);
  });

web3.eth.net.isListening()
.then(console.log);

/*
 * value - default is in MC =  ETH, 
 * in Sha, 1 mc = 1e+18 Sha
 * as in Wei, 1 eth = 1e+18 wei
*/
function sendTx(src, des, chainid, value){


web3.eth.getTransactionCount(src.key).then(
  txcount => {

    var rawTx = {
      from: src.addr,
      nonce: web3.utils.numberToHex(txcount),
      // 1 gwei
      gasPrice: web3.utils.numberToHex(400000000),//web3.intToHex(web3.eth.gasPrice),//web3.intToHex(400000000),
      gas: web3.utils.numberToHex(5000000),
      to: des.addr, 
      value: web3.utils.numberToHex(15000000000), 
      data: '0x00',
      chainId: chainid
    }
    
    web3.eth.accounts.signTransaction(rawTx, src["key"]).then( 
      value => {
        
        console.log("signed:", value);
        web3.eth.sendSignedTransaction(value.rawTransaction)
        .once('transactionHash', function(hash){ console.log("Get returned:",hash); });
      }, 
      reason => { console.error("Error with:", reason);});

  },
  reason => { console.error("Error with:", reason);});

}



return;



